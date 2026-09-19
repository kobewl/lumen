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

// MemoryCandidate 是一条记忆（候选或已有决策）。
//
// 与已确认记忆的区别：候选只是模型的提议，带来源与置信度，用户可以忽略；
// 只有 confirmed 的内容才可能进入后续轮次的上下文（且仅限偏好类，
// 通过 user_profile_entries 读模型注入，见 profile.go）。
//
// Key 是偏好类候选的稳定槽位名：用户纠正时，同一槽位的新确认值
// 替换旧值，而不是堆出两个互相矛盾的值。
type MemoryCandidate struct {
	ID         string
	UserID     string
	Kind       string
	Content    string
	Key        string
	SourceIDs  []string
	Confidence float64
	Status     string
	CreatedAt  time.Time
	// 决策信息：确认/拒绝由管理 API 写入，原始候选内容不改写。
	DecidedAt    time.Time
	DecidedBy    string
	RejectReason string
}

// SaveMemoryCandidate 写入一条候选记忆。
//
// 幂等：相同 ID 的候选不重复写入，避免重复提问时反复堆积。
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
		INSERT INTO memory_candidates (id, user_id, kind, content, key, source_ids, confidence, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		c.ID, c.UserID, c.Kind, c.Content, c.Key, string(sourceJSON), c.Confidence, status, FormatISO(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("写入记忆候选失败: %w", err)
	}
	return nil
}

// memoryCandidateColumns 是候选查询的公共列清单，避免多处 SELECT 漂移。
// decided_at 允许 NULL（尚未决策），统一 COALESCE 成空串方便扫描。
const memoryCandidateColumns = `id, user_id, kind, content, key, source_ids, confidence, status, created_at, COALESCE(decided_at, '') AS decided_at, decided_by, reject_reason`

// MemoryCandidateByID 按 ID 取一条候选；不存在时 ok=false。
func (s *Store) MemoryCandidateByID(ctx context.Context, id string) (MemoryCandidate, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT `+memoryCandidateColumns+` FROM memory_candidates WHERE id = ?`, id)
	c, err := scanMemoryRow(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryCandidate{}, false, nil
		}
		return MemoryCandidate{}, false, fmt.Errorf("查询记忆候选失败: %w", err)
	}
	return c, true, nil
}

// MemoryCandidatesByStatus 返回指定状态的候选（管理接口用，跨用户）。
//
// status 传 "all" 时返回全部状态，便于管理端一次看全生命周期。
func (s *Store) MemoryCandidatesByStatus(ctx context.Context, status string, limit int) ([]MemoryCandidate, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT ` + memoryCandidateColumns + ` FROM memory_candidates`
	args := []any{}
	if status != "" && status != "all" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询记忆候选失败: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// MemoryCandidatesByUser 返回用户的候选记忆（不含已确认与已拒绝的）。
func (s *Store) MemoryCandidatesByUser(ctx context.Context, userID string, limit int) ([]MemoryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT `+memoryCandidateColumns+`
		FROM memory_candidates
		WHERE user_id = ? AND status = ?
		ORDER BY created_at ASC, id ASC LIMIT ?`, userID, MemoryStatusCandidate, limit)
	if err != nil {
		return nil, fmt.Errorf("查询候选记忆失败: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// scanMemoryRow 把一行候选扫描成结构体，scan 目标函数由调用方给出
// （QueryRow 与 rows.Next 两种游标都能用）。
func scanMemoryRow(scan func(dest ...any) error) (MemoryCandidate, error) {
	var c MemoryCandidate
	var sourceJSON, created, decidedAt string
	if err := scan(&c.ID, &c.UserID, &c.Kind, &c.Content, &c.Key, &sourceJSON,
		&c.Confidence, &c.Status, &created, &decidedAt, &c.DecidedBy, &c.RejectReason); err != nil {
		return MemoryCandidate{}, err
	}
	_ = json.Unmarshal([]byte(sourceJSON), &c.SourceIDs)
	parsed, err := ParseISO(created)
	if err != nil {
		return MemoryCandidate{}, fmt.Errorf("解析记忆时间失败: %w", err)
	}
	c.CreatedAt = parsed
	if decidedAt != "" {
		if c.DecidedAt, err = ParseISO(decidedAt); err != nil {
			return MemoryCandidate{}, fmt.Errorf("解析决策时间失败: %w", err)
		}
	}
	return c, nil
}

// scanMemories 把查询结果扫描成记忆列表。
func scanMemories(rows *sql.Rows) ([]MemoryCandidate, error) {
	var out []MemoryCandidate
	for rows.Next() {
		c, err := scanMemoryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentTurn 是一轮近期对话（从 conversations 表投影），供上下文装配。
//
// 只取用户原话与当时的回答：这两样是解析代词真正需要的。
// 消息 ID 等技术字段不进模型上下文。
type RecentTurn struct {
	UserText   string
	AnswerText string
	At         time.Time
}

// RecentConversations 返回用户最近 N 轮对话（最新在前）。
//
// 数据来源是 conversations 表（问答记录本来就要落库），
// 不为"近期轮次"新建存储。查询失败由调用方降级处理。
func (s *Store) RecentConversations(ctx context.Context, userID string, limit int) ([]RecentTurn, error) {
	if limit <= 0 || limit > 20 {
		limit = 3
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT query_json, answer_text, created_at
		FROM conversations
		WHERE allowed_user_id = ? AND status = 'ok'
		ORDER BY created_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询近期对话失败: %w", err)
	}
	defer rows.Close()
	out := make([]RecentTurn, 0, limit)
	for rows.Next() {
		var queryJSON, answer, created string
		if err := rows.Scan(&queryJSON, &answer, &created); err != nil {
			return nil, fmt.Errorf("解析近期对话失败: %w", err)
		}
		var parsed struct {
			Raw string `json:"raw"`
		}
		_ = json.Unmarshal([]byte(queryJSON), &parsed)
		at, err := ParseISO(created)
		if err != nil {
			return nil, fmt.Errorf("解析近期对话时间失败: %w", err)
		}
		out = append(out, RecentTurn{UserText: parsed.Raw, AnswerText: answer, At: at})
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
