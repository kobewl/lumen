package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// migration 是一次有序的 schema 变更。
// 迁移通过 migrations 表记录已执行版本，重复运行不会重复执行。
type migration struct {
	version int
	name    string
	stmts   []string
}

// algorithmVersion 是当前 Session 聚合算法版本，写入 sessions 表用于追溯。
const algorithmVersion = "rules-v1"

// promptVersion 是当前总结 prompt 版本，写入 daily_summaries 表用于追溯。
const promptVersion = "summary-v1"

var migrations = []migration{
	{
		version: 1,
		name:    "initial schema",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS devices (
				id            TEXT PRIMARY KEY,
				name          TEXT NOT NULL,
				token_hash    TEXT NOT NULL,
				created_at    TEXT NOT NULL,
				last_seen_at  TEXT,
				revoked_at    TEXT
			)`,
			`CREATE TABLE IF NOT EXISTS events (
				id             TEXT PRIMARY KEY,
				device_id      TEXT NOT NULL,
				event_type     TEXT NOT NULL,
				timestamp      TEXT NOT NULL,
				received_at    TEXT NOT NULL,
				privacy_level  TEXT NOT NULL,
				context_json   TEXT NOT NULL DEFAULT '{}',
				data_json      TEXT NOT NULL DEFAULT '{}',
				batch_id       TEXT,
				clock_skew     INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_events_type_ts ON events(event_type, timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_events_device_ts ON events(device_id, timestamp)`,
			`CREATE TABLE IF NOT EXISTS sessions (
				id                TEXT PRIMARY KEY,
				date              TEXT NOT NULL,
				project           TEXT NOT NULL,
				start_at          TEXT NOT NULL,
				end_at            TEXT NOT NULL,
				apps_json         TEXT NOT NULL DEFAULT '[]',
				git_json          TEXT NOT NULL DEFAULT '[]',
				stats_json        TEXT NOT NULL DEFAULT '{}',
				algorithm_version TEXT NOT NULL,
				source_start_at   TEXT NOT NULL,
				source_end_at     TEXT NOT NULL,
				updated_at        TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_date ON sessions(date)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project, date)`,
			`CREATE TABLE IF NOT EXISTS daily_summaries (
				id                    TEXT PRIMARY KEY,
				date                  TEXT NOT NULL,
				status                TEXT NOT NULL,
				model                 TEXT,
				prompt_version        TEXT NOT NULL,
				input_hash            TEXT NOT NULL,
				source_session_ids_json TEXT NOT NULL DEFAULT '[]',
				structured_output_json  TEXT,
				rendered_text         TEXT,
				token_usage_json      TEXT,
				error_code            TEXT,
				created_at            TEXT NOT NULL,
				updated_at            TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_summaries_date ON daily_summaries(date, status)`,
			`CREATE TABLE IF NOT EXISTS notification_deliveries (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				summary_id   TEXT NOT NULL,
				provider     TEXT NOT NULL,
				target       TEXT,
				status       TEXT NOT NULL,
				attempts     INTEGER NOT NULL DEFAULT 0,
				last_error   TEXT,
				sent_at      TEXT,
				created_at   TEXT NOT NULL,
				updated_at   TEXT NOT NULL,
				UNIQUE(summary_id, provider)
			)`,
			`CREATE TABLE IF NOT EXISTS conversations (
				id                      TEXT PRIMARY KEY,
				feishu_message_id       TEXT NOT NULL,
				allowed_user_id         TEXT NOT NULL,
				intent                  TEXT NOT NULL,
				query_json              TEXT NOT NULL DEFAULT '{}',
				answer_text             TEXT,
				source_session_ids_json TEXT NOT NULL DEFAULT '[]',
				status                  TEXT NOT NULL,
				created_at              TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_message ON conversations(feishu_message_id)`,
			// ai_usage 用于每日调用次数与 token 预算控制，只记录计数，不记录请求内容。
			`CREATE TABLE IF NOT EXISTS ai_usage (
				date            TEXT NOT NULL,
				kind            TEXT NOT NULL,
				calls           INTEGER NOT NULL DEFAULT 0,
				prompt_tokens   INTEGER NOT NULL DEFAULT 0,
				completion_tokens INTEGER NOT NULL DEFAULT 0,
				updated_at      TEXT NOT NULL,
				PRIMARY KEY (date, kind)
			)`,
			// security_events 记录非白名单来源、非法字段等安全事件，不记录正文。
			`CREATE TABLE IF NOT EXISTS security_events (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				kind       TEXT NOT NULL,
				detail     TEXT NOT NULL,
				source     TEXT,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_security_created ON security_events(created_at)`,
		},
	},
}

// Migrate 执行所有未应用的迁移。已执行的迁移会被跳过，因此可重复调用。
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建 migrations 表失败: %w", err)
	}

	for _, m := range migrations {
		var exists int
		err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("检查迁移 %d 状态失败: %w", m.version, err)
		}
		if exists > 0 {
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("开启迁移事务失败: %w", err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("迁移 %d(%s) 执行失败: %w", m.version, m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, NowISO()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("记录迁移 %d 失败: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 %d 失败: %w", m.version, err)
		}
	}
	return nil
}
