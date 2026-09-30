"""本地存储与同步队列测试。

覆盖验收标准：
- 断网 30 分钟后事件仍在本地；
- 未同步事件不按保留期删除；
- 重复写入同一 event_id 不产生重复事件。
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

from lumen_desktop.event import STATUS_READY, STATUS_REJECTED, STATUS_SYNCED, Event
from lumen_desktop.storage import Storage
from lumen_desktop.ulid import new as new_ulid


def _storage(tmp_path) -> Storage:
    return Storage(tmp_path / "events.db")


def _window_event(when: datetime | None = None) -> Event:
    return Event.window_activity(
        event_id=new_ulid(),
        device_id="desktop-mac-01",
        start=when or datetime.now(timezone.utc),
        duration_seconds=60,
        app="Visual Studio Code",
        project="lumen",
    )


class TestStoreEvent:
    def test_stores_and_queues_event(self, tmp_path):
        store = _storage(tmp_path)
        try:
            event = _window_event()
            assert store.store_event(event) is True

            counts = store.counts()
            assert counts.get(STATUS_READY) == 1
            assert counts["queued"] == 1
        finally:
            store.close()

    def test_duplicate_event_id_is_ignored(self, tmp_path):
        store = _storage(tmp_path)
        try:
            event = _window_event()
            assert store.store_event(event) is True
            # 同一个 event_id 再写一次应返回 False，且不产生第二条。
            assert store.store_event(event) is False
            assert store.counts().get(STATUS_READY) == 1
        finally:
            store.close()

    def test_payload_excludes_forbidden_fields(self, tmp_path):
        store = _storage(tmp_path)
        try:
            store.store_event(_window_event())
            recent = store.recent_events(limit=1)
            assert len(recent) == 1
            payload = recent[0]
            for forbidden in ("window_title", "clipboard", "source_code", "path"):
                assert forbidden not in payload["context"]
                assert forbidden not in payload["data"]
            # 只有白名单命中的项目名会出现。
            assert payload["context"]["project"] == "lumen"
        finally:
            store.close()


class TestPendingEvents:
    def test_returns_events_in_time_order(self, tmp_path):
        store = _storage(tmp_path)
        try:
            base = datetime.now(timezone.utc)
            for offset in (5, 1, 3):
                store.store_event(_window_event(base - timedelta(minutes=offset)))
            pending = store.pending_events(limit=10)
            timestamps = [e.timestamp for e in pending]
            assert timestamps == sorted(timestamps)
        finally:
            store.close()

    def test_respects_limit_and_size(self, tmp_path):
        store = _storage(tmp_path)
        try:
            for _ in range(10):
                store.store_event(_window_event())
            assert len(store.pending_events(limit=3)) == 3
            # 上限设得极小也至少要返回一条，避免单条事件永远无法同步。
            assert len(store.pending_events(limit=10, max_bytes=10)) == 1
        finally:
            store.close()

    def test_retry_schedule_hides_event_temporarily(self, tmp_path):
        store = _storage(tmp_path)
        try:
            event = _window_event()
            store.store_event(event)
            store.schedule_retry([event.id], "batch-1", 60, "网络不可达")
            # 退避期内不应再次取到该事件。
            assert store.pending_events(limit=10) == []
            # 退避期结束后可再次取到。
            store.schedule_retry([event.id], "batch-1", -1, "重试")
            assert len(store.pending_events(limit=10)) == 1
            assert store.attempts_for(event.id) >= 2
        finally:
            store.close()


class TestSyncResultHandling:
    def test_mark_synced_removes_from_queue(self, tmp_path):
        store = _storage(tmp_path)
        try:
            event = _window_event()
            store.store_event(event)
            assert store.mark_synced([event.id], "batch-1") == 1
            assert store.counts().get(STATUS_SYNCED) == 1
            assert store.counts()["queued"] == 0
            assert store.pending_events(limit=10) == []
        finally:
            store.close()

    def test_mark_rejected_removes_from_queue_without_retry(self, tmp_path):
        store = _storage(tmp_path)
        try:
            event = _window_event()
            store.store_event(event)
            store.mark_rejected(event.id, "invalid_event")
            counts = store.counts()
            assert counts.get(STATUS_REJECTED) == 1
            assert counts["queued"] == 0
            assert store.pending_events(limit=10) == []
        finally:
            store.close()


class TestRetention:
    def test_cleanup_only_removes_old_synced_events(self, tmp_path):
        store = _storage(tmp_path)
        try:
            old = _window_event(datetime.now(timezone.utc) - timedelta(days=30))
            recent = _window_event()
            unsynced = _window_event()

            store.store_event(old)
            store.store_event(recent)
            store.store_event(unsynced)

            store.mark_synced([old.id], "batch-old")
            store.mark_synced([recent.id], "batch-new")
            # unsynced 保持待同步状态。

            # 把旧事件的同步时间伪装成 30 天前。
            with store._cursor() as cur:  # noqa: SLF001 - 测试需要直接改时间戳
                cur.execute(
                    "UPDATE events SET synced_at = ? WHERE id = ?",
                    ((datetime.now(timezone.utc) - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ"), old.id),
                )

            removed = store.cleanup_synced(retention_days=7)
            assert removed == 1
            remaining = store.counts()
            # 未同步事件绝不因保留期被删除。
            assert remaining.get(STATUS_READY) == 1
            assert remaining.get(STATUS_SYNCED) == 1
        finally:
            store.close()


class TestSettings:
    def test_settings_round_trip(self, tmp_path):
        store = _storage(tmp_path)
        try:
            store.set_setting("device_id", "desktop-mac-01")
            store.set_setting("paused", True)
            assert store.get_setting("device_id") == "desktop-mac-01"
            assert store.get_setting("paused") is True
            assert store.get_setting("missing", "fallback") == "fallback"
        finally:
            store.close()

    def test_meta_round_trip(self, tmp_path):
        store = _storage(tmp_path)
        try:
            store.set_meta("last_sync_at", "2026-09-17T10:00:00Z")
            assert store.get_meta("last_sync_at") == "2026-09-17T10:00:00Z"
        finally:
            store.close()
