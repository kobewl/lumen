package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 总结状态机。
const (
	SummaryPending = "pending"
	SummaryOK      = "succeeded"
	SummaryFailed  = "failed"
)

// SummaryStatusPending 别名，保持语义清晰。
const SummaryStatusPending = SummaryPending

// ErrSummaryExists 表示同日同输入的总结已成功生成，无需重复调用模型。
var ErrSummaryExists = errors.New("summary already succeeded")

// DailySummary 是某一日的结构化总结记录。
type DailySummary struct {
	ID               string
	Date             string
	Status           string
	Model            string
	PromptVersion    string
	InputHash        string
	SourceSessionIDs string
	StructuredOutput string
	RenderedText     string
	TokenUsage       string
	ErrorCode        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ClaimSummary 以“同日 + prompt 版本 + input_hash”为幂等键申请一次生成。
//
// 行为：
//   - 已有成功记录：返回 ErrSummaryExists；
//   - 已有 pending 记录：复用该行，允许重试；
//   - 已有 failed 记录：复用该行并重新置为 pending；
//   - 无记录：插入新的 pending 行。
func (s *Store) ClaimSummary(ctx context.Context, id, date, modelName, inputHash, sourceSessionIDs string) (DailySummary, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return DailySummary{}, fmt.Errorf("开启总结事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing DailySummary
	var created, updated string
	err = tx.QueryRowContext(ctx, `
		SELECT id, date, status, COALESCE(model, ''), prompt_version, input_hash,
		       source_session_ids_json, COALESCE(structured_output_json, ''), COALESCE(rendered_text, ''),
		       COALESCE(token_usage_json, ''), COALESCE(error_code, ''), created_at, updated_at
		FROM daily_summaries WHERE date = ? AND prompt_version = ? AND input_hash = ? LIMIT 1`,
		date, promptVersion, inputHash).
		Scan(&existing.ID, &existing.Date, &existing.Status, &existing.Model, &existing.PromptVersion,
			&existing.InputHash, &existing.SourceSessionIDs, &existing.StructuredOutput, &existing.RenderedText,
			&existing.TokenUsage, &existing.ErrorCode, &created, &updated)

	switch {
	case err == nil:
		if existing.Status == SummaryOK {
			return existing, ErrSummaryExists
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE daily_summaries SET status = ?, model = ?, updated_at = ? WHERE id = ?`,
			SummaryPending, modelName, NowISO(), existing.ID); err != nil {
			return DailySummary{}, fmt.Errorf("重置总结状态失败: %w", err)
		}
		existing.Status = SummaryPending
		existing.Model = modelName
		if err := tx.Commit(); err != nil {
			return DailySummary{}, err
		}
		return existing, nil

	case errors.Is(err, sql.ErrNoRows):
		now := NowISO()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO daily_summaries (id, date, status, model, prompt_version, input_hash,
				source_session_ids_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, date, SummaryPending, modelName, promptVersion, inputHash, sourceSessionIDs, now, now); err != nil {
			return DailySummary{}, fmt.Errorf("创建总结记录失败: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return DailySummary{}, err
		}
		createdAt, _ := ParseISO(now)
		return DailySummary{
			ID: id, Date: date, Status: SummaryPending, Model: modelName,
			PromptVersion: promptVersion, InputHash: inputHash,
			SourceSessionIDs: sourceSessionIDs, CreatedAt: createdAt, UpdatedAt: createdAt,
		}, nil

	default:
		return DailySummary{}, fmt.Errorf("查询总结记录失败: %w", err)
	}
}

// CompleteSummary 把总结标记为成功并保存结构化结果。
func (s *Store) CompleteSummary(ctx context.Context, id, structured, rendered, tokenUsage string) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE daily_summaries
		SET status = ?, structured_output_json = ?, rendered_text = ?, token_usage_json = ?,
		    error_code = NULL, updated_at = ?
		WHERE id = ?`, SummaryOK, structured, rendered, tokenUsage, NowISO(), id)
	if err != nil {
		return fmt.Errorf("保存总结结果失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("总结记录不存在: %s", id)
	}
	return nil
}

// FailSummary 把总结标记为失败，并记录错误码。绝不写入半截文本当成功结果。
func (s *Store) FailSummary(ctx context.Context, id, errorCode string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE daily_summaries SET status = ?, error_code = ?, updated_at = ? WHERE id = ?`,
		SummaryFailed, truncate(errorCode, 64), NowISO(), id)
	return err
}

// SummaryByInputHash 按「日期 + prompt 版本 + input_hash」查询总结记录。
//
// 用途：在调用模型之前先判断是否已有相同输入的成功结果。如果有，就只需要补发通知，
// 既不必再次调用模型，也不应该消耗当日预算。
func (s *Store) SummaryByInputHash(ctx context.Context, date, inputHash string) (DailySummary, bool, error) {
	var d DailySummary
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, date, status, COALESCE(model, ''), prompt_version, input_hash,
		       source_session_ids_json, COALESCE(structured_output_json, ''), COALESCE(rendered_text, ''),
		       COALESCE(token_usage_json, ''), COALESCE(error_code, ''), created_at, updated_at
		FROM daily_summaries
		WHERE date = ? AND prompt_version = ? AND input_hash = ?
		ORDER BY CASE status WHEN 'succeeded' THEN 0 ELSE 1 END, updated_at DESC
		LIMIT 1`, date, promptVersion, inputHash).
		Scan(&d.ID, &d.Date, &d.Status, &d.Model, &d.PromptVersion, &d.InputHash,
			&d.SourceSessionIDs, &d.StructuredOutput, &d.RenderedText, &d.TokenUsage,
			&d.ErrorCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DailySummary{}, false, nil
	}
	if err != nil {
		return DailySummary{}, false, fmt.Errorf("查询总结失败: %w", err)
	}
	if d.CreatedAt, err = ParseISO(created); err != nil {
		return DailySummary{}, false, err
	}
	if d.UpdatedAt, err = ParseISO(updated); err != nil {
		return DailySummary{}, false, err
	}
	return d, true, nil
}

// SummaryByDate 返回某天最近一条总结记录。
func (s *Store) SummaryByDate(ctx context.Context, date string) (DailySummary, bool, error) {
	var d DailySummary
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, date, status, COALESCE(model, ''), prompt_version, input_hash,
		       source_session_ids_json, COALESCE(structured_output_json, ''), COALESCE(rendered_text, ''),
		       COALESCE(token_usage_json, ''), COALESCE(error_code, ''), created_at, updated_at
		FROM daily_summaries WHERE date = ?
		ORDER BY CASE status WHEN 'succeeded' THEN 0 ELSE 1 END, updated_at DESC LIMIT 1`, date).
		Scan(&d.ID, &d.Date, &d.Status, &d.Model, &d.PromptVersion, &d.InputHash,
			&d.SourceSessionIDs, &d.StructuredOutput, &d.RenderedText, &d.TokenUsage,
			&d.ErrorCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DailySummary{}, false, nil
	}
	if err != nil {
		return DailySummary{}, false, fmt.Errorf("查询总结失败: %w", err)
	}
	if d.CreatedAt, err = ParseISO(created); err != nil {
		return DailySummary{}, false, err
	}
	if d.UpdatedAt, err = ParseISO(updated); err != nil {
		return DailySummary{}, false, err
	}
	return d, true, nil
}

// LastSummaryStatus 返回最近一次总结的状态与时间，用于健康检查。
func (s *Store) LastSummaryStatus(ctx context.Context) (status, date string, updatedAt time.Time, ok bool, err error) {
	var raw string
	err = s.DB.QueryRowContext(ctx, `
		SELECT status, date, updated_at FROM daily_summaries ORDER BY updated_at DESC LIMIT 1`).
		Scan(&status, &date, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, false, nil
	}
	if err != nil {
		return "", "", time.Time{}, false, err
	}
	updatedAt, err = ParseISO(raw)
	return status, date, updatedAt, true, err
}

// MarkDeliverySent 记录一次成功的通知投递（按 summary_id + provider 去重）。
func (s *Store) MarkDeliverySent(ctx context.Context, summaryID, provider, target string) error {
	now := NowISO()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO notification_deliveries (summary_id, provider, target, status, attempts, sent_at, created_at, updated_at)
		VALUES (?, ?, ?, 'sent', 1, ?, ?, ?)
		ON CONFLICT(summary_id, provider) DO UPDATE SET
			status = 'sent', attempts = notification_deliveries.attempts + 1,
			sent_at = excluded.sent_at, last_error = NULL, updated_at = excluded.updated_at`,
		summaryID, provider, target, now, now, now)
	return err
}

// MarkDeliveryFailed 记录一次失败的投递尝试。
func (s *Store) MarkDeliveryFailed(ctx context.Context, summaryID, provider, target, errMsg string) error {
	now := NowISO()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO notification_deliveries (summary_id, provider, target, status, attempts, last_error, created_at, updated_at)
		VALUES (?, ?, ?, 'failed', 1, ?, ?, ?)
		ON CONFLICT(summary_id, provider) DO UPDATE SET
			status = 'failed', attempts = notification_deliveries.attempts + 1,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
		summaryID, provider, target, truncate(errMsg, 200), now, now)
	return err
}

// DeliverySent 判断某个 summary 是否已经成功推送过，避免重复发送。
func (s *Store) DeliverySent(ctx context.Context, summaryID, provider string) (bool, error) {
	var status string
	err := s.DB.QueryRowContext(ctx, `
		SELECT status FROM notification_deliveries WHERE summary_id = ? AND provider = ? LIMIT 1`,
		summaryID, provider).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == "sent", nil
}

// RecordConversation 保存一次飞书问答所需的最小字段。
// 原始飞书事件 payload 不长期保存。
func (s *Store) RecordConversation(ctx context.Context, id, messageID, userID, intent, queryJSON, answerText, sourceIDs, status string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO conversations (id, feishu_message_id, allowed_user_id, intent, query_json, answer_text,
			source_session_ids_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(feishu_message_id) DO NOTHING`,
		id, messageID, userID, intent, queryJSON, truncate(answerText, 4000), sourceIDs, status, NowISO())
	return err
}

// ConversationExists 判断消息是否已处理过，保证重复投递只处理一次。
func (s *Store) ConversationExists(ctx context.Context, messageID string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM conversations WHERE feishu_message_id = ?`, messageID).Scan(&n)
	return n > 0, err
}

// ---- AI 预算与用量 ----

// AIUsage 是某类 AI 调用在某天的累计用量。
type AIUsage struct {
	Date             string
	Kind             string
	Calls            int
	PromptTokens     int
	CompletionTokens int
}

// AIUsageToday 读取当天某类调用的累计用量。
func (s *Store) AIUsageToday(ctx context.Context, date, kind string) (AIUsage, error) {
	var u AIUsage
	u.Date, u.Kind = date, kind
	err := s.DB.QueryRowContext(ctx, `
		SELECT calls, prompt_tokens, completion_tokens FROM ai_usage WHERE date = ? AND kind = ?`,
		date, kind).Scan(&u.Calls, &u.PromptTokens, &u.CompletionTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	return u, nil
}

// AddAIUsage 累加一次调用的用量。调用前应先检查预算。
func (s *Store) AddAIUsage(ctx context.Context, date, kind string, promptTokens, completionTokens int) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO ai_usage (date, kind, calls, prompt_tokens, completion_tokens, updated_at)
		VALUES (?, ?, 1, ?, ?, ?)
		ON CONFLICT(date, kind) DO UPDATE SET
			calls = ai_usage.calls + 1,
			prompt_tokens = ai_usage.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = ai_usage.completion_tokens + excluded.completion_tokens,
			updated_at = excluded.updated_at`,
		date, kind, promptTokens, completionTokens, NowISO())
	return err
}

// CountAIUsageCallsInRange 统计一段时间内的 AI 调用次数与 token 总量。
func (s *Store) CountAIUsageCallsInRange(ctx context.Context, from, to time.Time) (int, error) {
	var n sql.NullInt64
	// date 是本地时区日期字符串，这里按字符串范围比较即可。
	err := s.DB.QueryRowContext(ctx, `
		SELECT SUM(calls) FROM ai_usage WHERE date >= ? AND date <= ?`,
		from.Format("2006-01-02"), to.Format("2006-01-02")).Scan(&n)
	return int(n.Int64), err
}
