package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TaskSummary 是一条专业 Agent 汇报的任务摘要。
//
// 它是目前为止唯一带**结论**的数据源：其他事件只说明"用了什么应用、多久"，
// 只有这里记录"Agent 报告做完了什么"。因此回答"完成了什么"这类问题时，
// 依赖它的部分是 supported，而依赖应用时长的部分只能是 inferred。
type TaskSummary struct {
	// ID 是这条汇报事件的 event_id，保留用于回溯到原始事件。
	ID string
	// DeviceID 与 TaskID 共同构成幂等键。
	DeviceID string
	TaskID   string
	Project  string
	App      string
	Title    string
	Status   string
	Outcomes []string
	// OpenLoops 是 Agent 报告未完成的部分。
	OpenLoops []string
	// SourceAgent 是提交方标识（如 zcode-cli），用于区分来源。
	SourceAgent string
	// SourceSessionID 只用于追溯，绝不进入用户可见文本或模型上下文。
	SourceSessionID string
	OccurredAt      time.Time
	UpdatedAt       time.Time
}

// UpsertTaskSummary 幂等写入一条任务摘要。
//
// 同一个 (device_id, task_id) 重复汇报时覆盖旧内容并返回 updated=true：
// Agent 常在任务进行中和结束时各汇报一次，保留最早那条会让用户看到过期的
// "进行中"状态。返回的 created 用于区分"新任务"和"更新"，只影响日志。
func (s *Store) UpsertTaskSummary(ctx context.Context, t TaskSummary) (created bool, err error) {
	outcomesJSON, err := json.Marshal(nonNilStrings(t.Outcomes))
	if err != nil {
		return false, fmt.Errorf("序列化 outcomes 失败: %w", err)
	}
	openLoopsJSON, err := json.Marshal(nonNilStrings(t.OpenLoops))
	if err != nil {
		return false, fmt.Errorf("序列化 open_loops 失败: %w", err)
	}

	var existing string
	err = s.DB.QueryRowContext(ctx,
		`SELECT id FROM agent_task_summaries WHERE device_id = ? AND task_id = ?`,
		t.DeviceID, t.TaskID).Scan(&existing)
	switch {
	case err == nil:
		// 已存在：更新内容，但保留首次记录的 id 作为该任务的主键。
		if _, err := s.DB.ExecContext(ctx, `
			UPDATE agent_task_summaries SET
				project = ?, app = ?, title = ?, status = ?,
				outcomes_json = ?, open_loops_json = ?, source_agent = ?,
				source_session_id = ?, occurred_at = ?, updated_at = ?
			WHERE device_id = ? AND task_id = ?`,
			t.Project, t.App, t.Title, t.Status, string(outcomesJSON), string(openLoopsJSON),
			t.SourceAgent, t.SourceSessionID, FormatISO(t.OccurredAt), FormatISO(t.UpdatedAt),
			t.DeviceID, t.TaskID); err != nil {
			return false, fmt.Errorf("更新任务摘要失败: %w", err)
		}
		return false, nil
	default:
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO agent_task_summaries
				(id, device_id, task_id, project, app, title, status,
				 outcomes_json, open_loops_json, source_agent, source_session_id,
				 occurred_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(device_id, task_id) DO UPDATE SET
				project = excluded.project, app = excluded.app, title = excluded.title,
				status = excluded.status, outcomes_json = excluded.outcomes_json,
				open_loops_json = excluded.open_loops_json,
				source_agent = excluded.source_agent,
				source_session_id = excluded.source_session_id,
				occurred_at = excluded.occurred_at, updated_at = excluded.updated_at`,
			t.ID, t.DeviceID, t.TaskID, t.Project, t.App, t.Title, t.Status,
			string(outcomesJSON), string(openLoopsJSON), t.SourceAgent, t.SourceSessionID,
			FormatISO(t.OccurredAt), FormatISO(t.UpdatedAt)); err != nil {
			return false, fmt.Errorf("写入任务摘要失败: %w", err)
		}
		return true, nil
	}
}

// TaskSummariesBetween 返回时间区间内的任务摘要，按发生时间倒序。
//
// 区间按 occurred_at 过滤，左闭右开。
func (s *Store) TaskSummariesBetween(ctx context.Context, from, to time.Time, limit int) ([]TaskSummary, error) {
	return s.queryTaskSummaries(ctx, `
		SELECT id, device_id, task_id, project, app, title, status,
		       outcomes_json, open_loops_json, source_agent, source_session_id,
		       occurred_at, updated_at
		FROM agent_task_summaries
		WHERE occurred_at >= ? AND occurred_at < ?
		ORDER BY occurred_at DESC, task_id ASC
		LIMIT ?`, FormatISO(from), FormatISO(to), clampLimit(limit))
}

// TaskSummariesByProject 返回某个项目最近的任务摘要。
func (s *Store) TaskSummariesByProject(ctx context.Context, project string, limit int) ([]TaskSummary, error) {
	return s.queryTaskSummaries(ctx, `
		SELECT id, device_id, task_id, project, app, title, status,
		       outcomes_json, open_loops_json, source_agent, source_session_id,
		       occurred_at, updated_at
		FROM agent_task_summaries
		WHERE project = ?
		ORDER BY occurred_at DESC, task_id ASC
		LIMIT ?`, project, clampLimit(limit))
}

// KnownTaskProjects 返回最近有任务摘要的项目名，用于和时段项目对齐。
func (s *Store) KnownTaskProjects(ctx context.Context, from time.Time) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT project FROM agent_task_summaries
		WHERE occurred_at >= ? AND project != ''
		ORDER BY project ASC`, FormatISO(from))
	if err != nil {
		return nil, fmt.Errorf("查询任务项目失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("解析任务项目失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountTaskSummaries 统计任务摘要数量，用于健康检查。
func (s *Store) CountTaskSummaries(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM agent_task_summaries`).Scan(&n)
	return n, err
}

func (s *Store) queryTaskSummaries(ctx context.Context, query string, args ...any) ([]TaskSummary, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询任务摘要失败: %w", err)
	}
	defer rows.Close()

	var out []TaskSummary
	for rows.Next() {
		var t TaskSummary
		var outcomesJSON, openLoopsJSON, occurred, updated string
		if err := rows.Scan(&t.ID, &t.DeviceID, &t.TaskID, &t.Project, &t.App, &t.Title,
			&t.Status, &outcomesJSON, &openLoopsJSON, &t.SourceAgent, &t.SourceSessionID,
			&occurred, &updated); err != nil {
			return nil, fmt.Errorf("解析任务摘要失败: %w", err)
		}
		_ = json.Unmarshal([]byte(outcomesJSON), &t.Outcomes)
		_ = json.Unmarshal([]byte(openLoopsJSON), &t.OpenLoops)
		if t.OccurredAt, err = ParseISO(occurred); err != nil {
			return nil, fmt.Errorf("解析任务时间失败: %w", err)
		}
		if t.UpdatedAt, err = ParseISO(updated); err != nil {
			return nil, fmt.Errorf("解析任务更新时间失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// clampLimit 统一限制查询条数，避免一次拉出整表。
func clampLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// nonNilStrings 保证切片序列化成 []，而不是 null。
//
// nil 切片会被 json.Marshal 写成 null，读取方再解析出来是 nil 而不是空数组，
// 在"有没有结果"这种判断上会造成歧义。
func nonNilStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
