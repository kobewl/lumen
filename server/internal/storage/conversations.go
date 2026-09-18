package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ConversationState 是**有限的**跨轮上下文。
//
// 为什么不是完整聊天历史：把全部历史塞回模型既贵，又容易让模型把旧内容
// 当成新事实（尤其是用户随口的假设）。这里只保留解析代词真正需要的东西，
// 其余靠每轮的证据检索。
type ConversationState struct {
	UserID          string
	CurrentProject  string
	PendingQuestion string
	LastMode        string
	LastTimeRange   string
	UpdatedAt       time.Time
}

// ConversationStateByUser 读取用户的跨轮状态；不存在时返回零值而不是错误。
func (s *Store) ConversationStateByUser(ctx context.Context, userID string) (ConversationState, error) {
	var out ConversationState
	var updated string
	err := s.DB.QueryRowContext(ctx, `
		SELECT user_id, current_project, pending_question, last_mode, last_time_range, updated_at
		FROM conversation_states WHERE user_id = ?`, userID).
		Scan(&out.UserID, &out.CurrentProject, &out.PendingQuestion,
			&out.LastMode, &out.LastTimeRange, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConversationState{UserID: userID}, nil
		}
		return ConversationState{}, fmt.Errorf("读取对话状态失败: %w", err)
	}
	if out.UpdatedAt, err = ParseISO(updated); err != nil {
		return ConversationState{}, fmt.Errorf("解析对话状态时间失败: %w", err)
	}
	return out, nil
}

// SaveConversationState 覆盖写入用户的跨轮状态。
func (s *Store) SaveConversationState(ctx context.Context, st ConversationState) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO conversation_states
			(user_id, current_project, pending_question, last_mode, last_time_range, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			current_project = excluded.current_project,
			pending_question = excluded.pending_question,
			last_mode = excluded.last_mode,
			last_time_range = excluded.last_time_range,
			updated_at = excluded.updated_at`,
		st.UserID, st.CurrentProject, st.PendingQuestion, st.LastMode, st.LastTimeRange,
		FormatISO(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("写入对话状态失败: %w", err)
	}
	return nil
}

// 记忆候选的状态取值。
const (
	MemoryStatusCandidate = "candidate"
	MemoryStatusConfirmed = "confirmed"
	MemoryStatusRejected  = "rejected"
)

// MemoryCandidate 是一条**尚未确认**的记忆候选。
//
// 与已确认记忆的区别：候选只是模型的提议，带来源与置信度，用户可以忽略；
// 只有 confirmed 的内容才允许进入后续轮次的上下文。
type MemoryCandidate struct {
	ID         string
	UserID     string
	Kind       string
	Content    string
	SourceIDs  []string
	Confidence float64
	Status     string
	CreatedAt  time.Time
}

// SaveMemoryCandidate 写入一条候选记忆。
//
// 幂等：同一用户的相同内容只保留一条候选，避免重复提问时反复堆积。
func (s *Store) SaveMemoryCandidate(ctx context.Context, c MemoryCandidate) error {
	sourceJSON, err := json.Marshal(c.SourceIDs)
	if err != nil {
		sourceJSON = []byte("[]")
	}
	status := c.Status
	if status == "" {
		status = MemoryStatusCandidate
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO memory_candidates (id, user_id, kind, content, source_ids, confidence, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		c.ID, c.UserID, c.Kind, c.Content, string(sourceJSON), c.Confidence, status, FormatISO(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("写入记忆候选失败: %w", err)
	}
	return nil
}

// MemoryCandidatesByUser 返回用户的候选记忆（不含已确认与已拒绝的）。
//
// 目前只有测试与将来的"确认入口"会用到它：候选记忆刻意不参与回答上下文，
// 因此正常链路里没有读取点。
func (s *Store) MemoryCandidatesByUser(ctx context.Context, userID string, limit int) ([]MemoryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, kind, content, source_ids, confidence, status, created_at
		FROM memory_candidates
		WHERE user_id = ? AND status = ?
		ORDER BY created_at ASC, id ASC LIMIT ?`, userID, MemoryStatusCandidate, limit)
	if err != nil {
		return nil, fmt.Errorf("查询候选记忆失败: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// ConfirmedMemories 返回已确认的记忆，供后续轮次作为上下文。
//
// V0.1 没有确认入口，因此正常情况下返回空——这是刻意的：
// 未经用户确认的内容不进上下文。
func (s *Store) ConfirmedMemories(ctx context.Context, userID string, limit int) ([]MemoryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, kind, content, source_ids, confidence, status, created_at
		FROM memory_candidates
		WHERE user_id = ? AND status = ?
		ORDER BY created_at DESC LIMIT ?`, userID, MemoryStatusConfirmed, limit)
	if err != nil {
		return nil, fmt.Errorf("查询已确认记忆失败: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// scanMemories 把查询结果扫描成记忆列表。
func scanMemories(rows *sql.Rows) ([]MemoryCandidate, error) {
	var out []MemoryCandidate
	for rows.Next() {
		var c MemoryCandidate
		var sourceJSON, created string
		if err := rows.Scan(&c.ID, &c.UserID, &c.Kind, &c.Content, &sourceJSON,
			&c.Confidence, &c.Status, &created); err != nil {
			return nil, fmt.Errorf("解析记忆失败: %w", err)
		}
		_ = json.Unmarshal([]byte(sourceJSON), &c.SourceIDs)
		parsed, err := ParseISO(created)
		if err != nil {
			return nil, fmt.Errorf("解析记忆时间失败: %w", err)
		}
		c.CreatedAt = parsed
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountMemoryCandidates 统计候选记忆数量，用于健康检查与调试。
func (s *Store) CountMemoryCandidates(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memory_candidates WHERE status = ?`, MemoryStatusCandidate).Scan(&n)
	return n, err
}
