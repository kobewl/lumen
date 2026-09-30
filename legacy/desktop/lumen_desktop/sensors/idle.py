"""Idle 传感器：记录 active / idle / locked 状态区间。

事件语义（必须与服务端 sessions 引擎、protocol/README.md 保持一致）：

    一条 idle.state 描述**一段状态区间**，
    timestamp        = 区间开始时刻（UTC）
    data.state       = 该区间的状态（active / idle / locked）
    data.duration_seconds = 区间时长（秒）；缺省表示区间还在进行中（开放区间）

产出规则：
- 状态切换时先补发上一段区间（带时长），再为新状态发一条开放区间事件；
- idle 区间的起点取「最后一次输入」的时刻，而不是发现 idle 的时刻，
  这样服务端拿到的切断点就是用户真正停下来的时间；
- 检测到进程被系统挂起（睡眠）时，active 区间只算到最后一次观测时刻，
  绝不把睡眠时长算成工作或活跃时间；而 locked/idle 属于「用户不在」，
  区间可以跨越睡眠一直延续到下次解锁。

历史教训（真机证据 2026-09-17）：旧实现写的是 previous state + 变化时刻，
于 2026-09-18 01:14Z（本地 09:14 解锁时）才补出一条
`{"state":"active","duration_seconds":45094}`——把锁屏后的 12.5 小时
记成了 active，服务端因此无法正确切断 Session。
"""

from __future__ import annotations

import logging
import time
from datetime import datetime, timedelta, timezone

from ..config import Config
from ..event import Event
from ..ulid import new as new_ulid
from .base import Sensor
from .macos import QUARTZ_AVAILABLE, screen_is_locked, seconds_since_last_input

logger = logging.getLogger(__name__)

# 墙钟与单调钟的偏离超过这个下限（且超过若干轮询间隔）时，
# 认为进程在两次采样之间被系统挂起过（睡眠/休眠）。
SUSPEND_GAP_FLOOR_SECONDS = 60.0


class IdleSensor(Sensor):
    """空闲与锁屏状态采集。"""

    name = "idle"

    def __init__(self, config: Config, emit, on_state_change=None):
        super().__init__(emit)
        self.config = config
        # 状态变化时通知主程序：锁屏/idle 要停止窗口计时，恢复 active 再开始。
        self._on_state_change = on_state_change
        now = datetime.now(timezone.utc)
        self._state = "active"
        self._state_since = now
        self._last_tick_wall = now
        self._last_tick_mono = time.monotonic()
        self._baseline_done = False

    def _run(self) -> None:
        if not QUARTZ_AVAILABLE:
            logger.error("未安装 pyobjc/Quartz，Idle 传感器不可用")
            return

        poll = max(1.0, self.config.idle_poll_seconds)
        while not self.should_stop():
            try:
                now = datetime.now(timezone.utc)
                mono = time.monotonic()
                locked = screen_is_locked()
                idle_seconds = 0.0 if locked else seconds_since_last_input()
                self.tick(now, mono, locked, idle_seconds, poll)
            except Exception:
                logger.exception("Idle 传感器采样失败")

            if self._stop_event.wait(poll):
                break

    # ---- 采样与状态机 ----
    #
    # 采样参数全部由调用方传入，便于用真实时序（含系统睡眠）做回归测试。
    def tick(self, now: datetime, mono: float, locked: bool, idle_seconds: float,
             poll_seconds: float) -> None:
        """处理一次采样。"""
        suspended = self._was_suspended(now, mono, poll_seconds)
        boundary = self._last_tick_wall
        self._last_tick_wall = now
        self._last_tick_mono = mono

        if locked:
            new_state, started_at = "locked", now
        elif idle_seconds >= self.config.idle_threshold_seconds:
            # idle 区间从「最后一次输入」开始，而不是从发现它的时候开始。
            new_state, started_at = "idle", now - timedelta(seconds=max(0.0, idle_seconds))
        else:
            new_state, started_at = "active", now

        if not self._baseline_done:
            # 进程刚启动时不知道此前的状态，只建立基线，不产出事件。
            self._baseline_done = True
            self._state = new_state
            self._state_since = started_at
            return

        if new_state == self._state:
            return

        previous_state = self._state
        previous_since = self._state_since
        previous_end = started_at
        if suspended and previous_state == "active":
            # 挂起期间的空白不能算成活跃；最多只认到最后一次观测（再加一个轮询间隔）。
            previous_end = min(previous_end, boundary + timedelta(seconds=poll_seconds))
        if previous_end < previous_since:
            previous_end = previous_since

        self._state = new_state
        self._state_since = started_at
        logger.debug(
            "idle 状态变化: %s -> %s (上一段 %s ~ %s)",
            previous_state, new_state, previous_since, previous_end,
        )

        # 1) 结算上一段区间（带时长）；2) 为新状态发出开放区间。
        self._emit_interval(previous_state, previous_since, previous_end)
        self._emit_interval(new_state, started_at, None)

        if self._on_state_change:
            try:
                self._on_state_change(new_state, started_at)
            except Exception:
                logger.exception("idle 状态变化回调失败")

    def _was_suspended(self, now: datetime, mono: float, poll_seconds: float) -> bool:
        """判断两次采样之间进程是否被系统挂起过。

        墙钟在睡眠期间照常前进，单调钟不会，因此两者之差就是睡眠时长。
        """
        wall_delta = (now - self._last_tick_wall).total_seconds()
        mono_delta = mono - self._last_tick_mono
        gap = wall_delta - max(0.0, mono_delta)
        threshold = max(SUSPEND_GAP_FLOOR_SECONDS, 4 * max(1.0, poll_seconds))
        return gap >= threshold

    def _emit_interval(self, state: str, start: datetime, end: datetime | None) -> None:
        """产出一次状态区间事件；end 为 None 表示区间仍在进行中。"""
        duration = 0.0
        if end is not None:
            duration = max(0.0, (end - start).total_seconds())
            if duration < 1.0:
                # 不足 1 秒的区间没有信息量，不产生噪声事件。
                return
        event = Event.idle_state(
            event_id=new_ulid(),
            device_id=self.config.device_id,
            start=start,
            state=state,
            duration_seconds=duration,
        )
        self.emit(event)

    @property
    def current_state(self) -> str:
        """当前状态，供 status 命令展示。"""
        return self._state
