"""工作 → idle → 锁屏 → 过夜 → 解锁 的真实时序回归测试。

真机证据（2026-09-17）：用户 20:41 停止输入、20:42 锁屏，本地库里却出现
674 分钟的 loginwindow 活动，22:30 的飞书总结把锁屏后的时间算成了工作，
而当天真实使用只有 ZCode 22.7 分钟 + Tabbit 1.7 分钟。

这组测试把当晚的时序固定下来，覆盖三件事：
1. idle.state 的事件语义是「状态区间」：timestamp = 区间开始，duration = 区间时长，
   缺省表示区间还没结束；
2. 锁屏 / idle 后窗口计时必须停止，恢复 active 后才能重新开始；
3. loginwindow、屏保、认证窗口这类系统伪应用绝不能被计为工作。
"""

from __future__ import annotations

import time
from datetime import datetime, timedelta, timezone

import pytest

from lumen_desktop.config import Config
from lumen_desktop.event import Event, parse_rfc3339
from lumen_desktop.privacy import PrivacyFilter
from lumen_desktop.runtime import Runtime
from lumen_desktop.sensors.idle import IdleSensor
from lumen_desktop.sensors.macos import is_system_pseudo_app
from lumen_desktop.sensors.window import WindowSensor
from lumen_desktop.storage import Storage

# 当晚的锚点：2026-09-17 20:00（本地 UTC+8）= 12:00 UTC。
T0 = datetime(2026, 9, 17, 12, 0, 0, tzinfo=timezone.utc)

# ZCode 与 Tabbit 是当晚真实使用的两个应用。
ZCODE = ("ZCode", "dev.zcode.app")


class Recorder:
    """收集传感器产出的事件。"""

    def __init__(self) -> None:
        self.events: list[Event] = []

    def __call__(self, event: Event) -> None:
        self.events.append(event)

    def by_state(self, state: str) -> list[Event]:
        return [e for e in self.events if e.data.get("state") == state]

    def closed(self, state: str) -> list[Event]:
        """返回带时长的（已结束）状态区间事件。"""
        return [e for e in self.by_state(state) if "duration_seconds" in e.data]

    def open(self, state: str) -> list[Event]:
        """返回不带时长的（仍在进行）状态区间事件。"""
        return [e for e in self.by_state(state) if "duration_seconds" not in e.data]


class Timeline:
    """按真实时刻驱动 IdleSensor。

    默认墙钟与单调钟同步推进；frozen 参数用来模拟系统睡眠——
    墙钟继续走，单调钟停住（进程被挂起，采样不会发生）。
    """

    def __init__(self, sensor: IdleSensor) -> None:
        self.sensor = sensor
        # 单调钟从传感器当前值接着走，避免把真实启动时间差误判成系统睡眠。
        self.mono = sensor._last_tick_mono
        self.wall = sensor._last_tick_wall

    def tick(self, at: datetime, *, locked: bool = False, idle: float = 0.0,
             poll: float = 5.0, frozen: float = 0.0) -> None:
        wall_delta = (at - self.wall).total_seconds()
        self.mono += max(0.0, wall_delta - frozen)
        self.sensor.tick(at, self.mono, locked, idle, poll)
        self.wall = at


def make_idle_sensor(**overrides):
    """构造一个只记录事件的 IdleSensor。"""
    rec = Recorder()
    states: list[tuple[str, datetime]] = []

    def on_state_change(state: str, started_at: datetime) -> None:
        states.append((state, started_at))

    config = Config(device_id="test-device", **overrides)
    sensor = IdleSensor(config, rec, on_state_change=on_state_change)
    return sensor, rec, states


def make_window_sensor():
    rec = Recorder()
    config = Config(device_id="test-device")
    sensor = WindowSensor(config, PrivacyFilter({}), rec)
    return sensor, rec


class TestIdleStateSemantics:
    """idle.state 事件的区间语义。"""

    def test_first_sample_only_establishes_baseline(self):
        sensor, rec, _ = make_idle_sensor()
        Timeline(sensor).tick(T0, idle=3)
        assert rec.events == [], "进程刚启动时不应凭空产出状态事件"

    def test_idle_interval_starts_at_last_input(self):
        sensor, rec, _ = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=3)
        # 之后 300 秒没有输入：采样发生在 T0+300，区间其实从 T0 就开始了。
        tl.tick(T0 + timedelta(seconds=300), idle=300)

        idle_events = rec.by_state("idle")
        assert len(idle_events) == 1, f"进入 idle 应产出 1 条事件，实际 {len(idle_events)}"
        event = idle_events[0]
        assert event.timestamp == T0, "idle 区间必须从最后一次输入时刻开始，而不是发现时刻"
        assert "duration_seconds" not in event.data, "刚开始的区间是开放区间，不应携带时长"

    def test_lock_closes_active_interval_and_starts_locked_interval(self):
        sensor, rec, states = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=1)
        lock_at = T0 + timedelta(minutes=42)
        tl.tick(lock_at, locked=True, idle=0)

        active = rec.closed("active")
        assert len(active) == 1, "锁屏时必须结算上一段 active 区间"
        assert active[0].timestamp == T0
        assert active[0].data["duration_seconds"] == 42 * 60

        locked = rec.open("locked")
        assert len(locked) == 1, "锁屏必须立刻产出 locked 区间事件"
        assert locked[0].timestamp == lock_at
        assert states == [("locked", lock_at)], "运行时需要收到状态变化通知以停止窗口计时"

    def test_same_state_repeated_samples_do_not_emit(self):
        sensor, rec, _ = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=1)
        for i in range(1, 6):
            tl.tick(T0 + timedelta(minutes=i), idle=1)
        assert rec.by_state("idle") == []


class TestSleepHandling:
    """系统睡眠（进程被冻结）不能被算成任何状态区间。"""

    def test_sleep_does_not_extend_active_interval(self):
        sensor, rec, _ = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=1)
        last_seen = T0 + timedelta(minutes=1)
        tl.tick(last_seen, idle=1)

        # 一夜过去：墙钟跳 12 小时，但进程被挂起，采样直到早上才回来。
        wake = last_seen + timedelta(hours=12)
        tl.tick(wake, locked=True, frozen=12 * 3600)

        active = rec.closed("active")
        assert len(active) == 1
        duration = active[0].data["duration_seconds"]
        assert duration <= 2 * 60, f"睡眠期间不能计入 active 区间，实际 {duration} 秒"

    def test_locked_interval_spans_sleep_until_unlock(self):
        sensor, rec, states = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=1)
        lock_at = T0 + timedelta(minutes=42)
        tl.tick(lock_at, locked=True)

        # 锁屏后电脑睡眠，第二天早上解锁。
        unlock = lock_at + timedelta(hours=12)
        tl.tick(unlock, idle=2, frozen=12 * 3600)

        locked = rec.closed("locked")
        assert len(locked) == 1, "解锁时应结算 locked 区间"
        assert locked[0].timestamp == lock_at, "locked 区间的起点是锁屏时刻"
        assert locked[0].data["duration_seconds"] == 12 * 3600
        assert states == [("locked", lock_at), ("active", unlock)]


class TestFullNightTimeline:
    """当晚真实时序：结论必须是「20:42 之后没有工作」。"""

    def test_no_phantom_work_after_lock(self):
        sensor, rec, _ = make_idle_sensor()
        tl = Timeline(sensor)
        tl.tick(T0, idle=1)

        lock_at = T0 + timedelta(minutes=42, seconds=30)
        tl.tick(lock_at, locked=True)

        unlock = T0 + timedelta(hours=21, minutes=14)  # 次日 09:14（本地）
        tl.tick(unlock, idle=1, frozen=(unlock - lock_at).total_seconds())

        # active 区间只应覆盖锁屏前那段真实工作时间。
        total_active = sum(e.data["duration_seconds"] for e in rec.closed("active"))
        assert total_active == 42 * 60 + 30, f"active 区间总时长应为锁屏前的工作时间，实际 {total_active} 秒"

        locked = rec.closed("locked")
        assert locked and locked[0].data["duration_seconds"] == (unlock - lock_at).total_seconds()

        # 解锁之后必须有一个新的开放 active 区间，保证窗口计时能重新开始。
        assert rec.open("active"), "解锁后应重新开始采集"


class TestSystemPseudoApps:
    """系统伪应用识别。"""

    @pytest.mark.parametrize(
        "app,bundle",
        [
            ("loginwindow", "com.apple.loginwindow"),
            ("Loginwindow", ""),
            ("ScreenSaverEngine", "com.apple.ScreenSaver.Engine"),
            ("SecurityAgent", "com.apple.SecurityAgent"),
        ],
    )
    def test_pseudo_apps_detected(self, app, bundle):
        assert is_system_pseudo_app(app, bundle) is True

    @pytest.mark.parametrize(
        "app,bundle",
        [
            ("ZCode", "dev.zcode.app"),
            ("Tabbit浏览器", "com.tab-browser.Tabbit"),
            ("Finder", "com.apple.finder"),
            ("", ""),
        ],
    )
    def test_real_apps_not_filtered(self, app, bundle):
        assert is_system_pseudo_app(app, bundle) is False


class TestWindowSensorAway:
    """锁屏 / idle 期间窗口时长必须停止累计。"""

    def test_pseudo_app_never_produces_activity(self):
        sensor, rec = make_window_sensor()
        now = T0
        for _ in range(10):
            sensor._update_segment("loginwindow", "com.apple.loginwindow", "", now, 60.0)
            now += timedelta(seconds=60)
        assert rec.events == [], "loginwindow 不能被记录成工作活动"

    def test_pseudo_app_closes_previous_segment(self):
        sensor, rec = make_window_sensor()
        sensor._update_segment(*ZCODE, "", T0, 60.0)
        lock_at = T0 + timedelta(minutes=2)
        sensor._update_segment("loginwindow", "com.apple.loginwindow", "", lock_at, 60.0)

        assert len(rec.events) == 1
        assert rec.events[0].data["duration_seconds"] == 120, "上一段真实应用应在锁屏时刻关闭"

    def test_suspend_stops_accumulation_until_wake(self):
        sensor, rec = make_window_sensor()
        sensor._update_segment(*ZCODE, "", T0, 60.0)

        lock_at = T0 + timedelta(minutes=2)
        sensor.suspend(lock_at)
        assert len(rec.events) == 1, "挂起时应立即结算当前段"

        # 挂起期间继续轮询：不能再产生任何事件。
        for minute in range(1, 30):
            sensor._update_segment(*ZCODE, "", lock_at + timedelta(minutes=minute), 60.0)
        assert len(rec.events) == 1, "挂起期间不得继续累计窗口时长"

        wake = lock_at + timedelta(hours=12)
        sensor.wake()
        sensor._update_segment(*ZCODE, "", wake, 60.0)
        assert len(rec.events) == 1, "恢复后先开始新段，不应立即补发旧时长"

        sensor._close_segment(wake + timedelta(minutes=5))
        assert len(rec.events) == 2
        assert rec.events[1].data["duration_seconds"] == 5 * 60, "恢复后的时长必须从恢复时刻重新计算"

    def test_suspend_boundary_closes_segment_at_last_input(self):
        sensor, rec = make_window_sensor()
        sensor._update_segment(*ZCODE, "", T0, 60.0)
        # idle 起点（最后一次输入）早于发现 idle 的时刻。
        last_input = T0 + timedelta(minutes=5)
        sensor.suspend(last_input)
        assert rec.events[0].data["duration_seconds"] == 5 * 60


class TestRuntimeWiring:
    """Runtime 必须把 idle 状态接到窗口传感器上。"""

    def _runtime(self, tmp_path) -> Runtime:
        config = Config(device_id="test-device", db_path=str(tmp_path / "events.db"))
        storage = Storage(config.db_path)
        runtime = Runtime(config, storage, token="test-token")
        return runtime

    def test_lock_suspends_window_sensor_and_unlock_resumes(self, tmp_path):
        runtime = self._runtime(tmp_path)
        runtime.window_sensor._update_segment(*ZCODE, "", T0, 60.0)

        timeline = Timeline(runtime.idle_sensor)
        timeline.tick(T0, idle=1)
        lock_at = T0 + timedelta(minutes=3)
        timeline.tick(lock_at, locked=True)
        assert runtime.window_sensor.suspended is True, "锁屏后窗口传感器必须停止累计"

        timeline.tick(lock_at + timedelta(hours=10), idle=1)
        assert runtime.window_sensor.suspended is False, "解锁后必须恢复采集"
        runtime.storage.close()

    def test_idle_suspends_window_sensor_at_last_input(self, tmp_path):
        runtime = self._runtime(tmp_path)
        runtime.window_sensor._update_segment(*ZCODE, "", T0, 60.0)

        timeline = Timeline(runtime.idle_sensor)
        timeline.tick(T0, idle=2)
        # 300 秒没有输入：用户在 T0+300 停下，采样在 T0+600 才发现 idle。
        last_input = T0 + timedelta(seconds=300)
        runtime.window_sensor._update_segment(*ZCODE, "", last_input, 60.0)
        timeline.tick(T0 + timedelta(seconds=600), idle=300)
        assert runtime.window_sensor.suspended is True, "idle 后窗口传感器必须停止累计"

        events = runtime.storage.recent_events(limit=10)
        window_events = [e for e in events if e["type"] == "window.activity"]
        assert window_events, "离开前的工作时长应已落库"
        for event in window_events:
            start = parse_rfc3339(event["timestamp"])
            end = start + timedelta(seconds=event["data"]["duration_seconds"])
            assert end <= last_input, f"窗口活动不能越过最后活动时刻：{event}"
        assert sum(e["data"]["duration_seconds"] for e in window_events) == 300
        runtime.storage.close()

    def test_full_night_produces_no_loginwindow_activity(self, tmp_path):
        """端到端复现当晚：结论必须是没有任何登录窗口活动。"""
        runtime = self._runtime(tmp_path)
        timeline = Timeline(runtime.idle_sensor)

        # 20:00 开始在 ZCode 工作（每 60 秒一个 checkpoint）。
        timeline.tick(T0, idle=1)
        now = T0
        for _ in range(41):
            now += timedelta(seconds=60)
            runtime.window_sensor._update_segment(*ZCODE, "", now, 60.0)

        # 20:42 锁屏：此后前台变成 loginwindow，一直轮询到第二天早上。
        lock_at = T0 + timedelta(minutes=42)
        timeline.tick(lock_at, locked=True)
        now = lock_at
        for _ in range(700):
            now += timedelta(seconds=60)
            runtime.window_sensor._update_segment("loginwindow", "com.apple.loginwindow", "", now, 60.0)

        unlock = lock_at + timedelta(hours=12)
        timeline.tick(unlock, idle=1, frozen=(unlock - lock_at).total_seconds())
        runtime.window_sensor._update_segment(*ZCODE, "", unlock + timedelta(seconds=5), 60.0)
        runtime.window_sensor._close_segment(unlock + timedelta(minutes=2))

        runtime.storage.close()
        storage = Storage(runtime.config.db_path)
        events = storage.recent_events(limit=200)
        storage.close()

        apps = {e["context"].get("app") for e in events if e["type"] == "window.activity"}
        assert "loginwindow" not in apps, f"锁屏期间的假活动不得入库：{apps}"

        for event in events:
            if event["type"] != "window.activity":
                continue
            start = parse_rfc3339(event["timestamp"])
            end = start + timedelta(seconds=event["data"]["duration_seconds"])
            assert end <= lock_at or start >= unlock, (
                f"窗口活动落在了锁屏区间内：{event['context'].get('app')} "
                f"{start.isoformat()} ~ {end.isoformat()}"
            )


class TestWindowSensorSleepDetection:
    """没有锁屏、直接合盖睡眠时，窗口时长也不能把睡眠算进去。"""

    def test_sleep_does_not_extend_window_segment(self):
        """没锁屏直接合盖睡眠：整段睡眠绝不能被算成工作。"""
        sensor, rec = make_window_sensor()

        # 睡前：正常轮询，开始累计 ZCode。
        sensor.poll_once(T0, 500.0, 2.0, 60.0, *ZCODE, "", locked=False)
        last_seen = T0 + timedelta(minutes=10)
        sensor.poll_once(last_seen, 1100.0, 2.0, 60.0, *ZCODE, "", locked=False)

        # 醒来：墙钟跳 8 小时，单调钟只走了 5 秒。
        wake = last_seen + timedelta(hours=8)
        sensor.poll_once(wake, 1105.0, 2.0, 60.0, *ZCODE, "", locked=False)

        assert sensor.suspended is False, "恢复后应继续采集"
        total = sum(e.data["duration_seconds"] for e in rec.events)
        assert total <= 11 * 60, f"睡眠不能被计入窗口时长，实际 {total} 秒"

    def test_normal_poll_is_not_treated_as_sleep(self):
        sensor, rec = make_window_sensor()
        sensor.poll_once(T0, 100.0, 2.0, 60.0, *ZCODE, "", locked=False)
        sensor.poll_once(T0 + timedelta(seconds=2), 102.0, 2.0, 60.0, *ZCODE, "", locked=False)
        assert rec.events == [], "正常轮询不应切断活动段"

    def test_lock_poll_suspends_and_emits_nothing_after(self):
        sensor, rec = make_window_sensor()
        sensor.poll_once(T0, 200.0, 2.0, 60.0, *ZCODE, "", locked=False)
        lock_at = T0 + timedelta(minutes=3)
        sensor.poll_once(lock_at, 380.0, 2.0, 60.0, "loginwindow", "com.apple.loginwindow",
                         "", locked=True)
        assert sensor.suspended is True
        assert len(rec.events) == 1
        assert rec.events[0].data["duration_seconds"] == 180


class TestSyncErrorStatus:
    """同步状态里的 last_sync_error 必须反映**当前**状况。

    回归测试：它原来只在失败时写入、从不在成功后清除，于是一次网络抖动
    会让 status 永远显示「上次错误」，让人以为同步坏了。
    """

    def _runtime(self, tmp_path):
        config = Config(device_id="test-device", db_path=str(tmp_path / "events.db"))
        storage = Storage(config.db_path)
        runtime = Runtime(config, storage, token="test-token")
        return runtime

    def _stub(self, result):
        class Stub:
            def sync_once(self, force=False):
                return result

        return Stub()

    def test_successful_sync_clears_stale_error(self, tmp_path):
        from lumen_desktop.sync import SyncResult

        runtime = self._runtime(tmp_path)
        runtime.storage.set_meta("last_sync_error", "旧的网络错误")
        runtime.sync_client = self._stub(SyncResult(attempted=1, accepted=1, message="同步完成"))

        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error") is None, (
            "同步成功后应清除旧的错误记录")
        runtime.storage.close()

    def test_failed_sync_records_error(self, tmp_path):
        from lumen_desktop.sync import SyncResult

        runtime = self._runtime(tmp_path)
        runtime.sync_client = self._stub(
            SyncResult(attempted=1, retried=1, message="网络不可达"))

        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error") == "网络不可达"
        runtime.storage.close()

    def test_rejection_is_surfaced(self, tmp_path):
        """被拒事件意味着数据没能上报，必须让用户看得到。"""
        from lumen_desktop.sync import SyncResult

        runtime = self._runtime(tmp_path)
        runtime.sync_client = self._stub(SyncResult(
            attempted=1, rejected=1, message="同步结果: 1 条被拒绝",
            rejected_details=[{"event_id": "x", "code": "unknown_field"}]))

        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error"), "被拒事件应记录到状态里"
        runtime.storage.close()

    def test_skipped_without_token_keeps_error(self, tmp_path):
        """未注册是真实问题，错误记录必须保留。"""
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        runtime.storage.set_meta("last_sync_error", "未注册设备，缺少 device_token")
        # 用真实客户端（token 为空），验证 blocked 语义而不是桩数据。
        runtime.sync_client = SyncClient(runtime.config, runtime.storage, "")

        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error"), "未注册时应保留错误提示"
        runtime.storage.close()

    def test_stale_error_cleared_when_queue_empty(self, tmp_path):
        """队列空了，错误消息里的「已安排重试」就是假的，必须清掉。"""
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        runtime.storage.set_meta("last_sync_error", "网络不可达，已安排重试")
        runtime.sync_client = SyncClient(runtime.config, runtime.storage, "test-token")

        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error") is None, (
            "队列为空时不应继续显示「已安排重试」")
        runtime.storage.close()

    def test_pending_retry_keeps_error(self, tmp_path):
        """队列里还有等待退避重试的事件时，错误必须保留。"""
        from datetime import datetime, timezone
        from lumen_desktop.event import Event
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        event = Event.window_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B", device_id="test-device",
            start=datetime.now(timezone.utc), duration_seconds=60, app="ZCode")
        runtime.storage.store_event(event)
        runtime.storage.schedule_retry([event.id], "batch", 900.0, "网络不可达")
        runtime.storage.set_meta("last_sync_error", "网络不可达，已安排重试")

        runtime.sync_client = SyncClient(runtime.config, runtime.storage, "test-token")
        runtime._safe_sync(force=True)
        assert runtime.storage.get_meta("last_sync_error"), (
            "仍有待重试事件时不应清除错误")
        runtime.storage.close()


class TestSyncSkipSemantics:
    """`skipped` 有两种性质，必须区分对待。

    - 真实阻塞（未注册、队列硬上限）：要留在 status 里提醒用户；
    - 无事可做（队列为空）：错误消息里的「已安排重试」已经没有对应待办，应清掉。

    另外：隐私校验失败会把事件标为永久拒绝（丢弃），这属于数据没上报，
    必须在 status 里可见，不能静默跳过。
    """

    def _runtime(self, tmp_path):
        config = Config(device_id="test-device", db_path=str(tmp_path / "events.db"))
        storage = Storage(config.db_path)
        return Runtime(config, storage, token="test-token")

    def test_blocked_skip_is_marked(self, tmp_path):
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        # 未注册：token 为空。
        runtime.token = None
        client = SyncClient(runtime.config, runtime.storage, "")
        result = client.sync_once(force=True)
        assert result.skipped and result.blocked, "未注册属于真实阻塞，应标记 blocked"
        runtime.storage.close()

    def test_benign_skip_is_not_blocked(self, tmp_path):
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        client = SyncClient(runtime.config, runtime.storage, "test-token")
        result = client.sync_once(force=True)
        assert result.skipped and not result.blocked, "队列为空是无事可做，不是阻塞"
        runtime.storage.close()

    def test_privacy_rejection_is_counted_and_blocked(self, tmp_path):
        """隐私校验失败会丢事件，必须计入 rejected 并标为阻塞。"""
        import json
        from datetime import datetime, timezone
        from lumen_desktop.event import STATUS_READY
        from lumen_desktop.sync import SyncClient

        runtime = self._runtime(tmp_path)
        # 直接写库绕过 Event 的 allowlist：模拟历史脏数据里的绝对路径。
        with runtime.storage._cursor() as cur:
            cur.execute(
                """INSERT INTO events (id, type, timestamp, privacy, context_json, data_json,
                       status, created_at) VALUES (?,?,?,?,?,?,?,?)""",
                ("01J9Z4QK7M3F8N2P5R7T9V1X3C", "window.activity",
                 datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "P0",
                 json.dumps({"app": "Terminal", "project": "/Users/someone/secret"}),
                 json.dumps({"duration_seconds": 10}), STATUS_READY,
                 datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")),
            )
            cur.execute("INSERT INTO sync_queue (event_id, attempts) VALUES (?, 0)",
                        ("01J9Z4QK7M3F8N2P5R7T9V1X3C",))

        client = SyncClient(runtime.config, runtime.storage, "test-token")
        result = client.sync_once(force=True)
        assert result.skipped, "本批全部被拒时应跳过上传"
        assert result.rejected == 1, f"被拒事件必须计数，实际 {result.rejected}"
        assert result.blocked, "被拒事件是数据没上报，应标记 blocked"
        runtime.storage.close()
