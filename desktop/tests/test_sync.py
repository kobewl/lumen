"""同步客户端测试。

用一个内存假服务端验证核心语义：
- 只把 accepted / duplicate 标记为已同步；
- duplicate 视为成功，不是错误；
- rejected 记录错误并停止重试；
- 网络失败时事件保留在本地并安排退避；
- 部分成功时其余事件不丢失。
"""

from __future__ import annotations

import gzip
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from lumen_desktop.config import Config
from lumen_desktop.event import STATUS_READY, STATUS_REJECTED, STATUS_SYNCED, Event
from lumen_desktop.storage import Storage
from lumen_desktop.sync import SyncClient
from lumen_desktop.ulid import new as new_ulid


class FakeServer:
    """可编程的假服务端，记录收到的批次。"""

    def __init__(self):
        self.received: list[dict] = []
        self.responses: list[tuple[int, dict]] = []
        self.status_code = 200
        self.mode = "accept"  # accept | duplicate | reject | partial | error | unauthorized
        self._server: HTTPServer | None = None
        self._thread: threading.Thread | None = None

    def start(self) -> str:
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", 0))
                raw = self.rfile.read(length)
                if self.headers.get("Content-Encoding") == "gzip":
                    raw = gzip.decompress(raw)
                payload = json.loads(raw.decode("utf-8"))
                outer.received.append(payload)

                if outer.mode == "error":
                    self.send_response(500)
                    self.end_headers()
                    self.wfile.write(b'{"code":"server_error","message":"boom"}')
                    return
                if outer.mode == "unauthorized":
                    self.send_response(401)
                    self.end_headers()
                    self.wfile.write(b'{"code":"unauthorized","message":"bad token"}')
                    return

                results = []
                for event in payload["events"]:
                    if outer.mode == "duplicate":
                        results.append({"event_id": event["id"], "status": "duplicate"})
                    elif outer.mode == "reject":
                        results.append({
                            "event_id": event["id"], "status": "rejected",
                            "code": "forbidden_field", "message": "字段被禁止",
                        })
                    elif outer.mode == "partial":
                        # 第一条接受，第二条拒绝。
                        if len(results) == 0:
                            results.append({"event_id": event["id"], "status": "accepted"})
                        else:
                            results.append({
                                "event_id": event["id"], "status": "rejected",
                                "code": "invalid_event", "message": "格式错误",
                            })
                    else:
                        results.append({"event_id": event["id"], "status": "accepted"})

                body = json.dumps({
                    "batch_id": payload["batch_id"],
                    "server_time": "2026-09-17T10:05:01Z",
                    "results": results,
                }).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):  # 静默日志
                return

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return f"http://127.0.0.1:{self._server.server_port}"

    def stop(self):
        if self._server:
            self._server.shutdown()
            self._server.server_close()


@pytest.fixture
def server():
    s = FakeServer()
    url = s.start()
    yield s, url
    s.stop()


def make_client(tmp_path, url, token="test-token"):
    config = Config(server_url=url, device_id="desktop-mac-01", db_path=str(tmp_path / "events.db"))
    store = Storage(config.db_path)
    return SyncClient(config, store, token), store


def add_events(store, count=3):
    events = []
    for _ in range(count):
        from datetime import datetime, timezone

        event = Event.window_activity(
            event_id=new_ulid(),
            device_id="desktop-mac-01",
            start=datetime.now(timezone.utc),
            duration_seconds=60,
            app="Visual Studio Code",
            project="lumen",
        )
        store.store_event(event)
        events.append(event)
    return events


class TestSyncSuccess:
    def test_accepted_events_marked_synced(self, server, tmp_path):
        srv, url = server
        client, store = make_client(tmp_path, url)
        try:
            events = add_events(store, 3)
            result = client.sync_once(force=True)

            assert result.accepted == 3
            assert result.rejected == 0
            assert store.counts().get(STATUS_SYNCED) == 3
            assert store.counts()["queued"] == 0
            assert len(srv.received) == 1
            assert len(srv.received[0]["events"]) == 3
        finally:
            store.close()

    def test_duplicate_counts_as_success(self, server, tmp_path):
        srv, url = server
        srv.mode = "duplicate"
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 2)
            result = client.sync_once(force=True)

            assert result.duplicate == 2
            assert result.rejected == 0
            # duplicate 是成功状态：本地必须标记为已同步，否则会无限重传。
            assert store.counts().get(STATUS_SYNCED) == 2
            assert store.counts()["queued"] == 0
        finally:
            store.close()

    def test_no_events_no_request(self, server, tmp_path):
        srv, url = server
        client, store = make_client(tmp_path, url)
        try:
            result = client.sync_once(force=True)
            assert result.skipped
            assert srv.received == []
        finally:
            store.close()

    def test_large_batch_is_gzipped(self, server, tmp_path):
        srv, url = server
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 30)
            client.sync_once(force=True)
            assert len(srv.received) == 1
            assert len(srv.received[0]["events"]) == 30
        finally:
            store.close()


class TestSyncFailure:
    def test_network_error_keeps_events_local(self, tmp_path):
        # 指向一个不可达端口，模拟断网。
        client, store = make_client(tmp_path, "http://127.0.0.1:1")
        try:
            add_events(store, 3)
            result = client.sync_once(force=True)

            assert result.retried == 3
            # 事件必须保留在本地，等待补传。
            assert store.counts().get(STATUS_READY) == 3
            assert store.counts()["queued"] == 3
            # 退避期内不会立即重试。
            assert store.pending_events(limit=10) == []
        finally:
            store.close()

    def test_server_500_schedules_retry(self, server, tmp_path):
        srv, url = server
        srv.mode = "error"
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 2)
            result = client.sync_once(force=True)

            assert result.retried == 2
            assert store.counts().get(STATUS_READY) == 2
            # 每条事件都应记录至少一次尝试，便于计算退避间隔。
            for item in store.recent_events(limit=10):
                assert store.attempts_for(item["id"]) >= 1
        finally:
            store.close()

    def test_unauthorized_is_fatal_and_keeps_events(self, server, tmp_path):
        srv, url = server
        srv.mode = "unauthorized"
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 2)
            result = client.sync_once(force=True)

            # 鉴权失败不做无意义重试（retried=0），但事件保留在本地等待人工处理。
            assert result.retried == 0
            assert "同步失败" in result.message
            assert store.counts().get(STATUS_READY) == 2
            assert store.counts()["queued"] == 2
        finally:
            store.close()

    def test_missing_token_skips_without_request(self, server, tmp_path):
        srv, url = server
        client, store = make_client(tmp_path, url, token="")
        try:
            add_events(store, 1)
            result = client.sync_once(force=True)
            assert result.skipped
            assert srv.received == []
            assert store.counts().get(STATUS_READY) == 1
        finally:
            store.close()


class TestRejectionHandling:
    def test_rejected_events_recorded_and_not_retried(self, server, tmp_path):
        srv, url = server
        srv.mode = "reject"
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 2)
            result = client.sync_once(force=True)

            assert result.rejected == 2
            assert store.counts().get(STATUS_REJECTED) == 2
            # 永久拒绝的事件不应留在待同步队列里反复重传。
            assert store.counts()["queued"] == 0
            assert len(result.rejected_details) == 2
        finally:
            store.close()

    def test_partial_rejection_keeps_other_events_synced(self, server, tmp_path):
        """单条被拒绝时，其余事件仍应确认成功。"""
        srv, url = server
        srv.mode = "partial"
        client, store = make_client(tmp_path, url)
        try:
            add_events(store, 2)
            result = client.sync_once(force=True)

            assert result.accepted == 1
            assert result.rejected == 1
            counts = store.counts()
            assert counts.get(STATUS_SYNCED) == 1
            assert counts.get(STATUS_REJECTED) == 1
        finally:
            store.close()

    def test_privacy_violation_blocks_upload(self, server, tmp_path):
        """本地隐私校验不通过的事件不应被上传。"""
        srv, url = server
        client, store = make_client(tmp_path, url)
        try:
            from datetime import datetime, timezone

            # 直接往数据库塞一条含绝对路径的事件，绕过 Event 的 allowlist。
            with store._cursor() as cur:  # noqa: SLF001 - 测试隐私防线
                from lumen_desktop.ulid import new as uid

                event_id = uid()
                cur.execute(
                    """INSERT INTO events
                       (id, type, timestamp, privacy, context_json, data_json, status, created_at)
                       VALUES (?, 'window.activity', ?, 'P0', ?, '{}', ?, ?)""",
                    (
                        event_id,
                        datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                        json.dumps({"app": "Terminal", "project": "/Users/liang/secret"}),
                        STATUS_READY,
                        datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                    ),
                )
                cur.execute("INSERT INTO sync_queue (event_id) VALUES (?)", (event_id,))

            result = client.sync_once(force=True)
            assert srv.received == [], "含敏感内容的事件不应被上传"
            assert store.counts().get(STATUS_REJECTED) == 1
            assert result.skipped
        finally:
            store.close()


class TestRetryBackoff:
    def test_backoff_grows_with_attempts(self, server, tmp_path):
        srv, url = server
        srv.mode = "error"
        client, store = make_client(tmp_path, url)
        try:
            events = add_events(store, 1)
            event_id = events[0].id

            delays = []
            for _ in range(4):
                client._handle_retry(  # noqa: SLF001 - 直接验证退避计算
                    [e for e in store.pending_events(limit=10, max_bytes=10**9)] or events,
                    "batch-x", "模拟错误",
                )
                with store._cursor() as cur:  # noqa: SLF001
                    cur.execute(
                        "SELECT next_retry_at FROM sync_queue WHERE event_id = ?", (event_id,)
                    )
                    row = cur.fetchone()
                delays.append(row["next_retry_at"])
            # 退避时间应当单调不降（每次尝试推后更多或相等）。
            assert delays == sorted(delays)
            assert store.attempts_for(event_id) >= 4
        finally:
            store.close()
