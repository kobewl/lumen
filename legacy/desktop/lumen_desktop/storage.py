"""桌面端本地 SQLite 存储。

三张表（与设计文档一致）：
- events：事件正文、状态、创建与同步时间；
- sync_queue：event_id、batch_id、重试次数、下次重试时间、错误；
- settings：device_id、server_url、repo_roots、黑名单、paused。

事件状态机：created → ready_to_sync → synced；永久拒绝的事件进入 rejected。
device_token 不在这里，它存在 macOS Keychain。
"""

from __future__ import annotations

import json
import sqlite3
import threading
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterator

from .event import (
    STATUS_CREATED,
    STATUS_READY,
    STATUS_REJECTED,
    STATUS_SYNCED,
    Event,
    parse_rfc3339,
    utc_rfc3339,
)

SCHEMA = """
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    type        TEXT NOT NULL,
    timestamp   TEXT NOT NULL,
    privacy     TEXT NOT NULL,
    context_json TEXT NOT NULL DEFAULT '{}',
    data_json   TEXT NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    synced_at   TEXT,
    last_error  TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_status_ts ON events(status, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_type_ts ON events(type, timestamp);

CREATE TABLE IF NOT EXISTS sync_queue (
    event_id      TEXT PRIMARY KEY,
    batch_id      TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    next_retry_at TEXT,
    last_error    TEXT,
    FOREIGN KEY (event_id) REFERENCES events(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_queue_retry ON sync_queue(next_retry_at);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""


def _now() -> str:
    return utc_rfc3339(datetime.now(timezone.utc))


class Storage:
    """本地事件缓冲库。所有写操作串行化，避免 SQLite 写冲突。"""

    def __init__(self, db_path: str | Path):
        self.db_path = Path(db_path)
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(str(self.db_path), check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._configure()
        self._migrate()

    def _configure(self) -> None:
        """开启 WAL、busy_timeout 与外键，与服务端策略保持一致。"""
        with self._lock:
            self._conn.execute("PRAGMA journal_mode=WAL")
            self._conn.execute("PRAGMA busy_timeout=5000")
            self._conn.execute("PRAGMA foreign_keys=ON")
            self._conn.execute("PRAGMA synchronous=NORMAL")

    def _migrate(self) -> None:
        with self._lock:
            self._conn.executescript(SCHEMA)
            self._conn.commit()

    @contextmanager
    def _cursor(self) -> Iterator[sqlite3.Cursor]:
        with self._lock:
            cur = self._conn.cursor()
            try:
                yield cur
                self._conn.commit()
            except Exception:
                self._conn.rollback()
                raise
            finally:
                cur.close()

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    # ---- 事件写入 ----

    def store_event(self, event: Event, status: str = STATUS_READY) -> bool:
        """写入一条事件；event_id 已存在时返回 False（幂等）。"""
        payload = event.to_payload()
        now = _now()
        with self._cursor() as cur:
            try:
                cur.execute(
                    """INSERT INTO events
                       (id, type, timestamp, privacy, context_json, data_json, status, created_at)
                       VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                    (
                        event.id, event.type, payload["timestamp"], event.privacy,
                        json.dumps(payload["context"], ensure_ascii=False),
                        json.dumps(payload["data"], ensure_ascii=False),
                        status, now,
                    ),
                )
            except sqlite3.IntegrityError:
                return False

            if status == STATUS_READY:
                cur.execute(
                    "INSERT OR IGNORE INTO sync_queue (event_id, attempts) VALUES (?, 0)",
                    (event.id,),
                )
        return True

    # ---- 队列读取 ----

    def pending_events(self, limit: int = 100, max_bytes: int = 512 * 1024) -> list[Event]:
        """取出待同步事件，按时间顺序，同时受条数与字节数限制。"""
        now = _now()
        with self._cursor() as cur:
            cur.execute(
                """SELECT e.* FROM events e
                   JOIN sync_queue q ON q.event_id = e.id
                   WHERE e.status = ?
                     AND (q.next_retry_at IS NULL OR q.next_retry_at <= ?)
                   ORDER BY e.timestamp ASC
                   LIMIT ?""",
                (STATUS_READY, now, limit),
            )
            rows = cur.fetchall()

        out: list[Event] = []
        total = 0
        for row in rows:
            event = self._row_to_event(row)
            size = event.size_bytes()
            # 至少允许一条事件，避免单条超限导致永远无法同步。
            if out and total + size > max_bytes:
                break
            out.append(event)
            total += size
        return out

    def _row_to_event(self, row: sqlite3.Row) -> Event:
        return Event(
            id=row["id"],
            device_id=self.get_setting("device_id", "desktop-mac-01"),
            type=row["type"],
            timestamp=parse_rfc3339(row["timestamp"]),
            privacy=row["privacy"],
            context=json.loads(row["context_json"] or "{}"),
            data=json.loads(row["data_json"] or "{}"),
        )

    # ---- 同步结果处理 ----

    def mark_synced(self, event_ids: list[str], batch_id: str) -> int:
        """把事件标记为已同步，并移出队列。"""
        if not event_ids:
            return 0
        now = _now()
        with self._cursor() as cur:
            placeholders = ",".join("?" * len(event_ids))
            cur.execute(
                f"UPDATE events SET status = ?, synced_at = ?, last_error = NULL "
                f"WHERE id IN ({placeholders})",
                (STATUS_SYNCED, now, *event_ids),
            )
            affected = cur.rowcount
            cur.execute(
                f"DELETE FROM sync_queue WHERE event_id IN ({placeholders})",
                tuple(event_ids),
            )
        return affected

    def mark_rejected(self, event_id: str, error: str) -> None:
        """标记为永久拒绝：记录错误并移出队列，不再重试。"""
        with self._cursor() as cur:
            cur.execute(
                "UPDATE events SET status = ?, last_error = ? WHERE id = ?",
                (STATUS_REJECTED, error[:200], event_id),
            )
            cur.execute("DELETE FROM sync_queue WHERE event_id = ?", (event_id,))

    def schedule_retry(self, event_ids: list[str], batch_id: str, delay_seconds: float, error: str) -> None:
        """安排退避重试：增加尝试次数，设置下次可重试时间。"""
        if not event_ids:
            return
        next_at = utc_rfc3339(datetime.now(timezone.utc) + timedelta(seconds=delay_seconds))
        with self._cursor() as cur:
            for event_id in event_ids:
                cur.execute(
                    """INSERT INTO sync_queue (event_id, batch_id, attempts, next_retry_at, last_error)
                       VALUES (?, ?, 1, ?, ?)
                       ON CONFLICT(event_id) DO UPDATE SET
                           batch_id = excluded.batch_id,
                           attempts = sync_queue.attempts + 1,
                           next_retry_at = excluded.next_retry_at,
                           last_error = excluded.last_error""",
                    (event_id, batch_id, next_at, error[:200]),
                )

    def attempts_for(self, event_id: str) -> int:
        """查询某事件的已尝试次数，用于计算退避间隔。"""
        with self._cursor() as cur:
            cur.execute("SELECT attempts FROM sync_queue WHERE event_id = ?", (event_id,))
            row = cur.fetchone()
        return int(row["attempts"]) if row else 0

    # ---- 统计与清理 ----

    def counts(self) -> dict[str, int]:
        """返回各状态事件数，用于 status 命令与运维观察。"""
        with self._cursor() as cur:
            cur.execute("SELECT status, COUNT(1) AS n FROM events GROUP BY status")
            rows = cur.fetchall()
            cur.execute("SELECT COUNT(1) AS n FROM sync_queue")
            queued = cur.fetchone()["n"]
        out = {row["status"]: row["n"] for row in rows}
        out["queued"] = int(queued)
        out["total"] = sum(v for k, v in out.items() if k != "queued")
        return out

    def queue_size_bytes(self) -> int:
        """估算未同步数据占用的字节数，用于软/硬上限判断。"""
        with self._cursor() as cur:
            cur.execute(
                """SELECT COALESCE(SUM(LENGTH(context_json) + LENGTH(data_json) + 200), 0) AS n
                   FROM events WHERE status != ?""",
                (STATUS_SYNCED,),
            )
            return int(cur.fetchone()["n"])

    def cleanup_synced(self, retention_days: int) -> int:
        """删除超过保留期的已同步事件。未同步事件永不按时间删除。"""
        cutoff = utc_rfc3339(datetime.now(timezone.utc) - timedelta(days=retention_days))
        with self._cursor() as cur:
            cur.execute(
                "DELETE FROM events WHERE status = ? AND synced_at IS NOT NULL AND synced_at < ?",
                (STATUS_SYNCED, cutoff),
            )
            return cur.rowcount

    def recent_events(self, limit: int = 20) -> list[dict[str, Any]]:
        """返回最近事件供用户自查（“服务器实际收到了什么”）。"""
        with self._cursor() as cur:
            cur.execute(
                """SELECT id, type, timestamp, privacy, context_json, data_json, status
                   FROM events ORDER BY created_at DESC LIMIT ?""",
                (limit,),
            )
            rows = cur.fetchall()
        return [
            {
                "id": row["id"],
                "type": row["type"],
                "timestamp": row["timestamp"],
                "privacy": row["privacy"],
                "context": json.loads(row["context_json"] or "{}"),
                "data": json.loads(row["data_json"] or "{}"),
                "status": row["status"],
            }
            for row in rows
        ]

    # ---- settings ----

    def set_setting(self, key: str, value: Any) -> None:
        """写入设置项，值以 JSON 存储。"""
        with self._cursor() as cur:
            cur.execute(
                """INSERT INTO settings (key, value) VALUES (?, ?)
                   ON CONFLICT(key) DO UPDATE SET value = excluded.value""",
                (key, json.dumps(value, ensure_ascii=False)),
            )

    def get_setting(self, key: str, default: Any = None) -> Any:
        """读取设置项。"""
        with self._cursor() as cur:
            cur.execute("SELECT value FROM settings WHERE key = ?", (key,))
            row = cur.fetchone()
        if not row:
            return default
        try:
            return json.loads(row["value"])
        except json.JSONDecodeError:
            return default

    def set_meta(self, key: str, value: Any) -> None:
        """写入内部元数据（如上次同步时间）。"""
        with self._cursor() as cur:
            cur.execute(
                """INSERT INTO meta (key, value) VALUES (?, ?)
                   ON CONFLICT(key) DO UPDATE SET value = excluded.value""",
                (key, json.dumps(value, ensure_ascii=False)),
            )

    def get_meta(self, key: str, default: Any = None) -> Any:
        """读取内部元数据。"""
        with self._cursor() as cur:
            cur.execute("SELECT value FROM meta WHERE key = ?", (key,))
            row = cur.fetchone()
        if not row:
            return default
        try:
            return json.loads(row["value"])
        except json.JSONDecodeError:
            return default

    def checkpoint(self) -> None:
        """执行 WAL checkpoint，控制本地 WAL 文件增长。"""
        with self._lock:
            self._conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
