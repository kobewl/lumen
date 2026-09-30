package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// InitiativeOutbox 是一条主动关怀的出站记录（草稿 + 审计二合一）。
//
// status 取值见包内常量；text 是将发给用户的问题全文（用户本来就会看到），
// basis 是模型声称的依据，evidence 是本轮实际核实的依据——两者分开存，
// 事后能看出"模型说的依据"与"系统核实的依据"是否一致。
type InitiativeOutbox struct {
	ID         string
	UserID     string
	LocalDate  string
	CreatedAt  time.Time
	Status     string
	SkipReason string
	Text       string
	Basis      []string
	Evidence   []string
	Channel    string
	Delivered  time.Time
}

// initiative outbox 的状态取值。
const (
	// InitiativeStatusDraft：已生成待投递（渠道不可用或发送前暂存）。
	InitiativeStatusDraft = "draft"
	// InitiativeStatusDryRun：演练——走了完整流程但不真发。
	InitiativeStatusDryRun = "dry_run"
	// InitiativeStatusSent：已投递。
	InitiativeStatusSent = "sent"
	// InitiativeStatusFailed：投递失败（草稿内容保留，可重试）。
	InitiativeStatusFailed = "failed"
	// InitiativeStatusRejected：提案被代码校验拒绝或模型主动放弃。
	InitiativeStatusRejected = "rejected"
)

// SaveInitiativeOutbox 写入一条出站记录。
//
// 状态由调用方给（服务层负责状态机），存储层只做持久化与默认值兜底。
func (s *Store) SaveInitiativeOutbox(ctx context.Context, rec InitiativeOutbox) error {
	if rec.ID == "" {
		return fmt.Errorf("出站记录缺少 ID")
	}
	basisJSON, err := json.Marshal(rec.Basis)
	if err != nil {
		basisJSON = []byte("[]")
	}
	evidenceJSON, err := json.Marshal(rec.Evidence)
	if err != nil {
		evidenceJSON = []byte("[]")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	var delivered any
	if !rec.Delivered.IsZero() {
		delivered = FormatISO(rec.Delivered)
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO initiative_outbox
			(id, user_id, local_date, created_at, status, skip_reason, text,
			 basis_json, evidence_json, channel, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			skip_reason = excluded.skip_reason,
			text = excluded.text,
			channel = excluded.channel,
			delivered_at = excluded.delivered_at`,
		rec.ID, rec.UserID, rec.LocalDate, FormatISO(rec.CreatedAt),
		rec.Status, rec.SkipReason, rec.Text,
		string(basisJSON), string(evidenceJSON), rec.Channel, delivered)
	if err != nil {
		return fmt.Errorf("写入主动关怀出站记录失败: %w", err)
	}
	return nil
}

// CountInitiativeDeliveredToday 统计某用户当天已"发出"（sent + dry_run）的条数。
//
// dry_run 也计入：演练对用户而言同样是一次"如果真发会怎样"的打扰模拟，
// 让验收时的频率上限与生产行为一致。
func (s *Store) CountInitiativeDeliveredToday(ctx context.Context, userID, localDate string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM initiative_outbox
		WHERE user_id = ? AND local_date = ? AND status IN (?, ?)`,
		userID, localDate, InitiativeStatusSent, InitiativeStatusDryRun).Scan(&n)
	return n, err
}

// LastInitiativeAttemptAt 返回某用户最近一次**尝试**的时间（任意状态）。
//
// 间隔限制按"尝试"而不是"成功"计：模型跳过、提案被拒、投递失败都消耗了
// 一次打扰机会（和一次模型调用），否则调度器会以 30 分钟一次的频率
// 反复调用模型。
func (s *Store) LastInitiativeAttemptAt(ctx context.Context, userID string) (time.Time, bool, error) {
	var last string
	err := s.DB.QueryRowContext(ctx, `
		SELECT created_at FROM initiative_outbox
		WHERE user_id = ?
		ORDER BY created_at DESC LIMIT 1`,
		userID).Scan(&last)
	if err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("查询最近尝试失败: %w", err)
	}
	parsed, err := ParseISO(last)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("解析最近尝试时间失败: %w", err)
	}
	return parsed, true, nil
}

// RecentInitiativeOutbox 返回最近的出站记录（按创建倒序），供审计接口。
func (s *Store) RecentInitiativeOutbox(ctx context.Context, limit int) ([]InitiativeOutbox, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, local_date, created_at, status, skip_reason, text,
		       basis_json, evidence_json, channel, COALESCE(delivered_at, '')
		FROM initiative_outbox ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询出站记录失败: %w", err)
	}
	defer rows.Close()

	out := make([]InitiativeOutbox, 0, limit)
	for rows.Next() {
		var rec InitiativeOutbox
		var created, delivered string
		var basisJSON, evidenceJSON string
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.LocalDate, &created, &rec.Status,
			&rec.SkipReason, &rec.Text, &basisJSON, &evidenceJSON, &rec.Channel, &delivered); err != nil {
			return nil, fmt.Errorf("解析出站记录失败: %w", err)
		}
		if rec.CreatedAt, err = ParseISO(created); err != nil {
			return nil, fmt.Errorf("解析出站时间失败: %w", err)
		}
		if delivered != "" {
			if rec.Delivered, err = ParseISO(delivered); err != nil {
				return nil, fmt.Errorf("解析投递时间失败: %w", err)
			}
		}
		_ = json.Unmarshal([]byte(basisJSON), &rec.Basis)
		_ = json.Unmarshal([]byte(evidenceJSON), &rec.Evidence)
		out = append(out, rec)
	}
	return out, rows.Err()
}
