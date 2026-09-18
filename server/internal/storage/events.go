package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Event 是服务端保存的一条最小化事件。
type Event struct {
	ID           string
	DeviceID     string
	Type         string
	Timestamp    time.Time
	ReceivedAt   time.Time
	PrivacyLevel string
	ContextJSON  string
	DataJSON     string
	BatchID      string
	ClockSkew    bool
}

// InsertResult 描述一条事件的写入结果。
type InsertResult struct {
	Status string // accepted 或 duplicate
}

// InsertEvent 幂等写入单条事件。
// 返回 accepted（首次写入）或 duplicate（event_id 已存在），两者都视为成功。
func (s *Store) InsertEvent(ctx context.Context, e Event) (InsertResult, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO events (id, device_id, event_type, timestamp, received_at, privacy_level, context_json, data_json, batch_id, clock_skew)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		e.ID, e.DeviceID, e.Type, FormatISO(e.Timestamp), FormatISO(e.ReceivedAt),
		e.PrivacyLevel, e.ContextJSON, e.DataJSON, e.BatchID, boolToInt(e.ClockSkew))
	if err != nil {
		return InsertResult{}, fmt.Errorf("写入事件失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return InsertResult{}, fmt.Errorf("读取写入结果失败: %w", err)
	}
	if affected == 0 {
		return InsertResult{Status: "duplicate"}, nil
	}
	return InsertResult{Status: "accepted"}, nil
}

// EventsBetween 返回时间区间内的事件，用于 Session 聚合。
// 区间为左闭右开 [from, to)。
func (s *Store) EventsBetween(ctx context.Context, from, to time.Time) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, device_id, event_type, timestamp, received_at, privacy_level, context_json, data_json, COALESCE(batch_id, ''), clock_skew
		FROM events
		WHERE timestamp >= ? AND timestamp < ?
		ORDER BY timestamp ASC, id ASC`,
		FormatISO(from), FormatISO(to))
	if err != nil {
		return nil, fmt.Errorf("查询事件失败: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var ts, recv string
		var skew int
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.Type, &ts, &recv, &e.PrivacyLevel,
			&e.ContextJSON, &e.DataJSON, &e.BatchID, &skew); err != nil {
			return nil, fmt.Errorf("解析事件失败: %w", err)
		}
		if e.Timestamp, err = ParseISO(ts); err != nil {
			return nil, fmt.Errorf("解析事件时间失败: %w", err)
		}
		if e.ReceivedAt, err = ParseISO(recv); err != nil {
			return nil, fmt.Errorf("解析接收时间失败: %w", err)
		}
		e.ClockSkew = skew != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEvents 统计事件总数，用于健康检查。
func (s *Store) CountEvents(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM events`).Scan(&n)
	return n, err
}

// LastEventReceivedAt 返回最近一次事件的接收时间。
func (s *Store) LastEventReceivedAt(ctx context.Context) (time.Time, bool, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT received_at FROM events ORDER BY received_at DESC LIMIT 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := ParseISO(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// DeleteEventsBefore 小批量删除过期事件，返回删除条数。
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 5000
	}
	// 使用短事务小批量删除，避免长时间占用写锁。
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM events WHERE id IN (
			SELECT id FROM events WHERE timestamp < ? LIMIT ?
		)`, FormatISO(cutoff), batch)
	if err != nil {
		return 0, fmt.Errorf("删除过期事件失败: %w", err)
	}
	return res.RowsAffected()
}

// CountPendingEvents 返回尚未生成 Session 依赖统计所需的当日事件数。
func (s *Store) CountEventsInRange(ctx context.Context, from, to time.Time) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM events WHERE timestamp >= ? AND timestamp < ?`,
		FormatISO(from), FormatISO(to)).Scan(&n)
	return n, err
}

// TableStats 返回各表行数，用于健康检查与运维观察。
func (s *Store) TableStats(ctx context.Context) (map[string]int64, error) {
	tables := []string{"events", "sessions", "daily_summaries", "devices", "conversations"}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		// 表名来自内部白名单常量，不存在注入风险。
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+t).Scan(&n); err != nil {
			return nil, fmt.Errorf("统计表 %s 失败: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders 生成 n 个 SQL 占位符，供 IN 查询使用。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
