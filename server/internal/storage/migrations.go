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
	{
		version: 2,
		name:    "agent runtime: conversation state and memory candidates",
		stmts: []string{
			// conversation_states 保存**有限的**跨轮上下文：当前项目、待澄清问题、
			// 上一轮的模式与时间范围。刻意不保存完整聊天历史——
			// 把全部对话塞回模型既贵又容易让模型把旧内容当成事实。
			`CREATE TABLE IF NOT EXISTS conversation_states (
				user_id          TEXT PRIMARY KEY,
				current_project  TEXT NOT NULL DEFAULT '',
				pending_question TEXT NOT NULL DEFAULT '',
				last_mode        TEXT NOT NULL DEFAULT '',
				last_time_range  TEXT NOT NULL DEFAULT '',
				updated_at       TEXT NOT NULL
			)`,
			// memory_candidates 只保存**候选**记忆，绝不直接成为已确认记忆。
			// 晋升必须由用户确认（V0.1 尚无确认入口，因此全部停留在 candidate）。
			`CREATE TABLE IF NOT EXISTS memory_candidates (
				id         TEXT PRIMARY KEY,
				user_id    TEXT NOT NULL,
				kind       TEXT NOT NULL,
				content    TEXT NOT NULL,
				source_ids TEXT NOT NULL DEFAULT '[]',
				confidence REAL NOT NULL DEFAULT 0,
				status     TEXT NOT NULL DEFAULT 'candidate',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_memory_user_status ON memory_candidates(user_id, status)`,
		},
	},
	{
		version: 3,
		name:    "agent task summaries",
		stmts: []string{
			// agent_task_summaries 是专业 Agent 主动汇报的任务摘要。
			//
			// 为什么单独建表而不是从 events 里现查：任务摘要是**可更新**的实体
			// （同一个 task_id 可能被重复汇报，后来者覆盖前者），需要按
			// (device_id, task_id) 做幂等 upsert；events 表是 append-only 的
			// 不可变事件流，语义不同，混在一起会让两边的保证都变模糊。
			//
			// 原始 event 仍然照常写入 events 表，因此审计链完整：
			// 这张表是投影，events 是事实来源。
			`CREATE TABLE IF NOT EXISTS agent_task_summaries (
				id                TEXT PRIMARY KEY,
				device_id         TEXT NOT NULL,
				task_id           TEXT NOT NULL,
				project           TEXT NOT NULL DEFAULT '',
				app               TEXT NOT NULL DEFAULT '',
				title             TEXT NOT NULL,
				status            TEXT NOT NULL,
				outcomes_json     TEXT NOT NULL DEFAULT '[]',
				open_loops_json   TEXT NOT NULL DEFAULT '[]',
				source_agent      TEXT NOT NULL,
				source_session_id TEXT NOT NULL DEFAULT '',
				occurred_at       TEXT NOT NULL,
				updated_at        TEXT NOT NULL
			)`,
			// 幂等键：同一设备上的同一个任务只保留最新一条汇报。
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_task_summaries_task
				ON agent_task_summaries(device_id, task_id)`,
			`CREATE INDEX IF NOT EXISTS idx_task_summaries_occurred
				ON agent_task_summaries(occurred_at)`,
			`CREATE INDEX IF NOT EXISTS idx_task_summaries_project
				ON agent_task_summaries(project, occurred_at)`,
		},
	},
	{
		version: 4,
		name:    "agent tool audits",
		stmts: []string{
			// tool_audits 是工具调用的审计账本：谁请求的、调了什么、带什么参数、
			// 放行还是拒绝、为什么、耗时多久、返回了多少条。
			//
			// 刻意**不记录结果内容**：结果可能含用户数据，而审计要回答的是
			// "这一轮发生了什么"，不是"用户的数据长什么样"。
			// 参数值写入前会被裁剪（只保留前 64 字），因此这里也不会堆积全文。
			`CREATE TABLE IF NOT EXISTS tool_audits (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				at          TEXT NOT NULL,
				actor       TEXT NOT NULL DEFAULT '',
				tool        TEXT NOT NULL,
				risk        TEXT NOT NULL DEFAULT '',
				args_json   TEXT NOT NULL DEFAULT '{}',
				decision    TEXT NOT NULL,
				reason      TEXT NOT NULL DEFAULT '',
				duration_ms INTEGER NOT NULL DEFAULT 0,
				result_kind TEXT NOT NULL DEFAULT '',
				item_count  INTEGER NOT NULL DEFAULT 0,
				evidence_n  INTEGER NOT NULL DEFAULT 0,
				truncated   INTEGER NOT NULL DEFAULT 0
			)`,
			// 按时间倒序查最近调用是最常见的审计动作。
			`CREATE INDEX IF NOT EXISTS idx_tool_audits_at ON tool_audits(at)`,
			// 按工具与决策统计：用来发现"模型在反复尝试越权"这类模式。
			`CREATE INDEX IF NOT EXISTS idx_tool_audits_tool ON tool_audits(tool, decision)`,
		},
	},
	{
		version: 5,
		name:    "initiative outbox",
		stmts: []string{
			// initiative_outbox 是主动关怀的出站账本（出站草稿 + 审计二合一）：
			// 谁在什么时候、凭什么证据、想说什么、最后发没发出去。
			//
			// 刻意**不存**活动明细快照：text 是将发给用户的问题全文（用户本来
			// 就会看到），evidence/basis 只存可核实的记录 ID。
			// local_date 是本地时区日期，"每天最多 2 次"按它计数。
			`CREATE TABLE IF NOT EXISTS initiative_outbox (
				id           TEXT PRIMARY KEY,
				user_id      TEXT NOT NULL,
				local_date   TEXT NOT NULL,
				created_at   TEXT NOT NULL,
				status       TEXT NOT NULL,
				skip_reason  TEXT NOT NULL DEFAULT '',
				text         TEXT NOT NULL DEFAULT '',
				basis_json   TEXT NOT NULL DEFAULT '[]',
				evidence_json TEXT NOT NULL DEFAULT '[]',
				channel      TEXT NOT NULL DEFAULT '',
				delivered_at TEXT
			)`,
			`CREATE INDEX IF NOT EXISTS idx_initiative_user_date
				ON initiative_outbox(user_id, local_date)`,
			`CREATE INDEX IF NOT EXISTS idx_initiative_created
				ON initiative_outbox(created_at)`,
		},
	},
	{
		version: 6,
		name:    "memory lifecycle and user profile read model",
		stmts: []string{
			// 记忆生命周期：给候选补上决策列。确认/拒绝只追加决策信息，
			// **不改写**原始候选内容——候选正文永远保持模型当时写下的样子，
			// 谁在什么时候决定了什么，从这一行就能完整回溯。
			//
			// key 是偏好类候选的**稳定槽位名**（如"称呼/作息"）：用户纠正时，
			// 新确认值替换同一槽位的旧值，而不是堆出两个互相矛盾的值。
			// 旧库候选 key 为空串，确认时落到 general 槽位。
			`ALTER TABLE memory_candidates ADD COLUMN key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE memory_candidates ADD COLUMN decided_at TEXT`,
			`ALTER TABLE memory_candidates ADD COLUMN decided_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE memory_candidates ADD COLUMN reject_reason TEXT NOT NULL DEFAULT ''`,
			// user_profile_entries 是**已确认信息**的读模型（当前值）：
			// 每个用户每个槽位一行。上下文装配器只读这张表（和摘要/轮次），
			// 接口层面没有"读候选记忆"的路径，候选正文因此不可能泄漏进模型。
			`CREATE TABLE IF NOT EXISTS user_profile_entries (
				user_id             TEXT NOT NULL,
				key                 TEXT NOT NULL,
				kind                TEXT NOT NULL DEFAULT 'preference',
				value               TEXT NOT NULL,
				version             INTEGER NOT NULL DEFAULT 1,
				source_candidate_id TEXT NOT NULL DEFAULT '',
				source_ids_json     TEXT NOT NULL DEFAULT '[]',
				created_at          TEXT NOT NULL,
				updated_at          TEXT NOT NULL,
				PRIMARY KEY (user_id, key)
			)`,
			// user_profile_history 是版本历史（兼确认动作审计）：追加式，
			// 记录每次确认的槽位、新旧值、来源候选与操作者。
			// 纠正时旧值从这里找回——entries 只留当前值。
			`CREATE TABLE IF NOT EXISTS user_profile_history (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id        TEXT NOT NULL,
				key            TEXT NOT NULL,
				version        INTEGER NOT NULL,
				action         TEXT NOT NULL,
				candidate_id   TEXT NOT NULL DEFAULT '',
				value          TEXT NOT NULL,
				previous_value TEXT NOT NULL DEFAULT '',
				operator       TEXT NOT NULL DEFAULT 'admin',
				created_at     TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_profile_history_user
				ON user_profile_history(user_id, key, created_at)`,
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
