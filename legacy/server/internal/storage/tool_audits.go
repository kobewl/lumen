package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"lumen/server/internal/tooling"
)

// ToolAudit 是一条已落库的工具调用审计记录。
//
// 它与 tooling.AuditRecord 的区别：这里是存储层的形状（时间是字符串、JSON 已序列化），
// 而上层用的是领域形状。转换只在本文件发生。
type ToolAudit struct {
	ID         int64
	At         time.Time
	Actor      string
	Tool       string
	Risk       string
	Args       map[string]any
	Decision   string
	Reason     string
	DurationMS int64
	ResultKind string
	ItemCount  int
	EvidenceN  int
	Truncated  bool
}

// ToolAuditSink 把 tooling 的审计接口接到 SQLite。
//
// 它是"审计写入点"的生产实现：Executor 只依赖 tooling.AuditSink 接口，
// 因此换存储不影响工具与执行逻辑。
type ToolAuditSink struct {
	Store *Store
}

// Record 写入一条审计。
func (s ToolAuditSink) Record(ctx context.Context, rec tooling.AuditRecord) error {
	if s.Store == nil {
		return fmt.Errorf("审计写入缺少存储")
	}
	argsJSON, err := json.Marshal(rec.Args)
	if err != nil || len(argsJSON) == 0 {
		argsJSON = []byte("{}")
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	truncated := 0
	if rec.Truncated {
		truncated = 1
	}
	_, err = s.Store.DB.ExecContext(ctx, `
		INSERT INTO tool_audits
			(at, actor, tool, risk, args_json, decision, reason,
			 duration_ms, result_kind, item_count, evidence_n, truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		FormatISO(at), rec.Actor, rec.Tool, string(rec.Risk), string(argsJSON),
		rec.Decision, rec.Reason, rec.DurationMS, string(rec.ResultKind),
		rec.Count, rec.EvidenceN, truncated)
	if err != nil {
		return fmt.Errorf("写入工具审计失败: %w", err)
	}
	return nil
}

// RecentToolAudits 返回最近的工具审计（按时间倒序），用于只读调试接口。
func (s *Store) RecentToolAudits(ctx context.Context, limit int) ([]ToolAudit, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, at, actor, tool, risk, args_json, decision, reason,
		       duration_ms, result_kind, item_count, evidence_n, truncated
		FROM tool_audits ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询工具审计失败: %w", err)
	}
	defer rows.Close()

	out := make([]ToolAudit, 0, limit)
	for rows.Next() {
		var rec ToolAudit
		var at, argsJSON string
		var truncated int
		if err := rows.Scan(&rec.ID, &at, &rec.Actor, &rec.Tool, &rec.Risk, &argsJSON,
			&rec.Decision, &rec.Reason, &rec.DurationMS, &rec.ResultKind,
			&rec.ItemCount, &rec.EvidenceN, &truncated); err != nil {
			return nil, fmt.Errorf("解析工具审计失败: %w", err)
		}
		if rec.At, err = ParseISO(at); err != nil {
			return nil, fmt.Errorf("解析工具审计时间失败: %w", err)
		}
		rec.Args = map[string]any{}
		_ = json.Unmarshal([]byte(argsJSON), &rec.Args)
		rec.Truncated = truncated == 1
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountToolAudits 统计审计条数，用于健康检查。
func (s *Store) CountToolAudits(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM tool_audits`).Scan(&n)
	return n, err
}
