// Package summary 编排每日总结：组装上下文 → 调用模型 → 校验 → 持久化 → 推送。
//
// 失败隔离：模型失败只把该条总结标记为 failed，不影响事件、Session 和查询；
// 推送失败也只标记 delivery 状态，总结本身已保存，可补发。
package summary

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/notification"
	"lumen/server/internal/sessions"
	"lumen/server/internal/storage"
	"lumen/server/internal/ulid"
)

// Service 负责每日总结的生成与推送。
type Service struct {
	store          *storage.Store
	engine         *sessions.Engine
	client         *ai.Client
	sender         notification.Sender
	loc            *time.Location
	summaryLimit   int
	logger         *slog.Logger
	allowedUserIDs []string
}

// NewService 创建总结服务。
func NewService(store *storage.Store, engine *sessions.Engine, client *ai.Client, sender notification.Sender,
	loc *time.Location, summaryLimit int, allowedUserIDs []string, logger *slog.Logger) *Service {
	if loc == nil {
		loc = time.UTC
	}
	if summaryLimit <= 0 {
		summaryLimit = 2
	}
	return &Service{
		store: store, engine: engine, client: client, sender: sender,
		loc: loc, summaryLimit: summaryLimit, allowedUserIDs: allowedUserIDs, logger: logger,
	}
}

// SetSenderForTest 替换通知渠道，仅供测试使用。
//
// 生产代码不会调用它：真实渠道在 NewService 时注入并保持不变。
func (s *Service) SetSenderForTest(sender notification.Sender, allowedUserIDs []string) {
	s.sender = sender
	s.allowedUserIDs = allowedUserIDs
}

// Result 是一次生成尝试的结果。
type Result struct {
	Date      string
	SummaryID string
	Status    string
	Text      string
	Skipped   string
}

// GenerateForDate 为指定日期生成总结。
//
// 幂等策略：
//   - 先重算该日 Session，保证总结基于最新事件；
//   - 用 input_hash 作为幂等键，输入相同且已成功则直接返回，不重复调用模型；
//   - 输入变化（例如补传了迟到事件）时允许重新生成。
func (s *Service) GenerateForDate(ctx context.Context, date string) (Result, error) {
	day, err := time.ParseInLocation("2006-01-02", date, s.loc)
	if err != nil {
		return Result{}, errors.New("日期格式必须是 YYYY-MM-DD")
	}

	// 生成前重算 Session，确保包含已到达的迟到事件。
	if _, err := s.engine.RebuildDay(ctx, day); err != nil {
		return Result{}, err
	}

	sess, err := s.store.SessionsByDate(ctx, date)
	if err != nil {
		return Result{}, err
	}
	if len(sess) == 0 {
		return Result{Date: date, Status: "skipped", Skipped: "no_sessions"}, nil
	}

	in := ai.BuildInput(date, s.loc, sess)
	inputHash := ai.InputHash(in)
	sourceIDs := ai.SourceSessionIDs(in)
	sourceJSON := marshalStrings(sourceIDs)

	// 先查是否已有相同输入的成功结果。
	//
	// 这一步必须在预算检查之前：已有结果时只需要补发通知，既不该重复调用模型，
	// 也不该因为"当日预算已用完"而拒绝补发。飞书一度推送失败、或用户手工触发
	// 重发时，都会走到这里。
	if existing, ok, err := s.store.SummaryByInputHash(ctx, date, inputHash); err != nil {
		return Result{}, err
	} else if ok && existing.Status == storage.SummaryOK {
		if existing.RenderedText != "" {
			s.Deliver(ctx, existing.ID, existing.RenderedText)
		}
		return Result{
			Date: date, SummaryID: existing.ID, Status: "already_succeeded",
			Text: existing.RenderedText,
		}, nil
	}

	if !s.client.Enabled() {
		return Result{Date: date, Status: "skipped", Skipped: "ai_disabled"}, nil
	}

	// 预算检查：每日总结调用次数上限。
	usage, err := s.store.AIUsageToday(ctx, date, "summary")
	if err != nil {
		return Result{}, err
	}
	if usage.Calls >= s.summaryLimit {
		return Result{Date: date, Status: "skipped", Skipped: "budget_exhausted"}, nil
	}

	record, err := s.store.ClaimSummary(ctx, ulid.New(), date, s.client.Model(), inputHash, sourceJSON)
	if err != nil {
		if errors.Is(err, storage.ErrSummaryExists) {
			// 并发情况下被别的请求抢先完成：复用结果并确保已推送。
			if record.RenderedText != "" {
				s.Deliver(ctx, record.ID, record.RenderedText)
			}
			return Result{Date: date, SummaryID: record.ID, Status: "already_succeeded", Text: record.RenderedText}, nil
		}
		return Result{}, err
	}

	structured, rendered, resp, err := ai.GenerateSummary(ctx, s.client, in)
	if err != nil {
		code := errorCode(err)
		if err := s.store.FailSummary(ctx, record.ID, code); err != nil {
			s.logger.Error("标记总结失败状态出错", "summary_id", record.ID, "error", err.Error())
		}
		// 即使失败也计入调用次数，避免失败重试无限消耗预算。
		_ = s.store.AddAIUsage(ctx, date, "summary", 0, 0)
		s.logger.Warn("总结生成失败", "date", date, "summary_id", record.ID, "code", code)
		return Result{Date: date, SummaryID: record.ID, Status: storage.SummaryFailed}, nil
	}

	if err := s.store.CompleteSummary(ctx, record.ID, ai.MarshalStructured(structured), rendered, ai.MarshalUsage(resp)); err != nil {
		return Result{}, err
	}
	if err := s.store.AddAIUsage(ctx, date, "summary", resp.Usage.PromptTokens, resp.Usage.CompletionTokens); err != nil {
		s.logger.Warn("记录 token 用量失败", "error", err.Error())
	}
	s.logger.Info("总结生成成功", "date", date, "summary_id", record.ID,
		"prompt_tokens", resp.Usage.PromptTokens, "completion_tokens", resp.Usage.CompletionTokens,
		"elapsed_ms", resp.Elapsed.Milliseconds())

	// 推送是尽力而为：失败不影响总结保存。
	s.Deliver(ctx, record.ID, rendered)

	return Result{Date: date, SummaryID: record.ID, Status: storage.SummaryOK, Text: rendered}, nil
}

// Deliver 把总结推送给允许用户，同一 summary_id 不重复推送。
func (s *Service) Deliver(ctx context.Context, summaryID, text string) bool {
	if s.sender == nil || !s.sender.Enabled() || len(s.allowedUserIDs) == 0 {
		return false
	}

	sent, err := s.store.DeliverySent(ctx, summaryID, s.sender.Name())
	if err != nil {
		s.logger.Warn("查询推送状态失败", "error", err.Error())
	}
	if sent {
		return true
	}

	var lastErr error
	for _, userID := range s.allowedUserIDs {
		if err := s.sender.SendText(ctx, userID, text); err != nil {
			lastErr = err
			continue
		}
		if err := s.store.MarkDeliverySent(ctx, summaryID, s.sender.Name(), userID); err != nil {
			s.logger.Warn("记录推送状态失败", "error", err.Error())
		}
		s.logger.Info("总结已推送", "summary_id", summaryID, "target", maskID(userID))
		return true
	}

	if lastErr != nil {
		_ = s.store.MarkDeliveryFailed(ctx, summaryID, s.sender.Name(), "", lastErr.Error())
		s.logger.Warn("总结推送失败", "summary_id", summaryID, "error", lastErr.Error())
	}
	return false
}

// RedeliverLatest 在飞书恢复后补发最近的总结，用于手工运维。
func (s *Service) RedeliverLatest(ctx context.Context, date string) (bool, error) {
	record, ok, err := s.store.SummaryByDate(ctx, date)
	if err != nil {
		return false, err
	}
	if !ok || record.Status != storage.SummaryOK || record.RenderedText == "" {
		return false, nil
	}
	return s.Deliver(ctx, record.ID, record.RenderedText), nil
}

// errorCode 把内部错误映射为可写入数据库的短错误码，避免把长文本或敏感信息落库。
func errorCode(err error) string {
	var apiErr *ai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "日期不符"):
		return "date_mismatch"
	case strings.Contains(msg, "JSON"):
		return "invalid_json"
	case strings.Contains(msg, "headline"):
		return "missing_field"
	default:
		return "validation_failed"
	}
}

// maskID 只保留用户标识首尾少量字符，避免日志留存完整账号。
func maskID(id string) string {
	if len(id) <= 6 {
		return "***"
	}
	return id[:3] + "***" + id[len(id)-3:]
}

func marshalStrings(in []string) string {
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}
