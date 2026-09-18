package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Session 是一段按规则聚合出的连续工作时段。
type Session struct {
	ID               string
	Date             string // 本地时区日期 YYYY-MM-DD
	Project          string
	StartAt          time.Time
	EndAt            time.Time
	AppsJSON         string
	GitJSON          string
	StatsJSON        string
	AlgorithmVersion string
	SourceStartAt    time.Time
	SourceEndAt      time.Time
	UpdatedAt        time.Time
}

// ReplaceSessions 用一次事务替换某日的全部 Session。
// 规则：先删除该日期已有 Session，再写入新结果，保证重算幂等。
//
// 删除按 date 而不是按时间区间：Session 的 date 是它「开始时刻所在的本地
// 日期」，重算某天就是完整重建这一天，按区间删除会留下早期版本写下的、
// 起点落在别处的残留行。
func (s *Store) ReplaceSessions(ctx context.Context, date string, from, to time.Time, sessions []Session) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 Session 事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE date = ?`, date); err != nil {
		return fmt.Errorf("清理旧 Session 失败: %w", err)
	}

	for _, sess := range sessions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, date, project, start_at, end_at, apps_json, git_json, stats_json,
				algorithm_version, source_start_at, source_end_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				project = excluded.project,
				start_at = excluded.start_at,
				end_at = excluded.end_at,
				apps_json = excluded.apps_json,
				git_json = excluded.git_json,
				stats_json = excluded.stats_json,
				algorithm_version = excluded.algorithm_version,
				source_start_at = excluded.source_start_at,
				source_end_at = excluded.source_end_at,
				updated_at = excluded.updated_at`,
			sess.ID, sess.Date, sess.Project, FormatISO(sess.StartAt), FormatISO(sess.EndAt),
			sess.AppsJSON, sess.GitJSON, sess.StatsJSON, sess.AlgorithmVersion,
			FormatISO(sess.SourceStartAt), FormatISO(sess.SourceEndAt), FormatISO(sess.UpdatedAt)); err != nil {
			return fmt.Errorf("写入 Session 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 Session 事务失败: %w", err)
	}
	return nil
}

// SessionsByDate 返回某天（本地时区）的全部 Session，按开始时间排序。
func (s *Store) SessionsByDate(ctx context.Context, date string) ([]Session, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, date, project, start_at, end_at, apps_json, git_json, stats_json,
		       algorithm_version, source_start_at, source_end_at, updated_at
		FROM sessions WHERE date = ? ORDER BY start_at ASC`, date)
	if err != nil {
		return nil, fmt.Errorf("查询 Session 失败: %w", err)
	}
	defer rows.Close()
	return scanSessions(rows)
}

// SessionsByProject 返回某项目最近若干天的 Session，用于“项目 X 最近做了什么”问答。
func (s *Store) SessionsByProject(ctx context.Context, project string, limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, date, project, start_at, end_at, apps_json, git_json, stats_json,
		       algorithm_version, source_start_at, source_end_at, updated_at
		FROM sessions WHERE project = ? ORDER BY start_at DESC LIMIT ?`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("查询项目 Session 失败: %w", err)
	}
	defer rows.Close()
	return scanSessions(rows)
}

// ProjectsWithActivity 返回指定日期区间内出现过的项目名，用于项目问答的模糊匹配。
func (s *Store) ProjectsWithActivity(ctx context.Context, from, to time.Time) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT project FROM sessions
		WHERE start_at >= ? AND start_at < ? ORDER BY project ASC`,
		FormatISO(from), FormatISO(to))
	if err != nil {
		return nil, fmt.Errorf("查询项目列表失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountSessions 返回 Session 总数。
func (s *Store) CountSessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM sessions`).Scan(&n)
	return n, err
}

func scanSessions(rows *sql.Rows) ([]Session, error) {
	var out []Session
	for rows.Next() {
		var sess Session
		var start, end, srcStart, srcEnd, updated string
		if err := rows.Scan(&sess.ID, &sess.Date, &sess.Project, &start, &end,
			&sess.AppsJSON, &sess.GitJSON, &sess.StatsJSON, &sess.AlgorithmVersion,
			&srcStart, &srcEnd, &updated); err != nil {
			return nil, fmt.Errorf("解析 Session 失败: %w", err)
		}
		var err error
		if sess.StartAt, err = ParseISO(start); err != nil {
			return nil, err
		}
		if sess.EndAt, err = ParseISO(end); err != nil {
			return nil, err
		}
		if sess.SourceStartAt, err = ParseISO(srcStart); err != nil {
			return nil, err
		}
		if sess.SourceEndAt, err = ParseISO(srcEnd); err != nil {
			return nil, err
		}
		if sess.UpdatedAt, err = ParseISO(updated); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
