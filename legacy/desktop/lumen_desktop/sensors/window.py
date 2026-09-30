"""Window 传感器：记录前台应用、起止时间与时长。

关键设计：
- 窗口标题默认**不采集、不上传**。只有在命中项目白名单关键词时，
  才把命中的项目名作为 context.project 上传，标题本身始终丢弃。
- 应用不切换时持续累加时长，每 60 秒落一次 checkpoint；
  应用切换或暂停/锁屏时关闭上一段活动。
- 未授予辅助功能权限时降级为只记录应用名。
- 锁屏 / idle / 系统伪应用（loginwindow、屏保、认证窗口）期间**不累计**任何时长，
  恢复 active 后才重新开始。两层防御：idle 传感器通知 + 本传感器自己探测锁屏。

依赖 pyobjc 的 AppKit 获取前台应用。macOS 上获取窗口标题需要辅助功能权限，
拿不到时静默降级，不报错。
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from ..config import Config
from ..event import Event
from ..privacy import PrivacyFilter
from ..ulid import new as new_ulid
from .base import Sensor
from .macos import is_system_pseudo_app, screen_is_locked

logger = logging.getLogger(__name__)

# 墙钟与单调钟的偏离超过这个下限（且超过若干轮询间隔）时，
# 认为进程在两次轮询之间被系统挂起过（睡眠/休眠）。
SUSPEND_GAP_FLOOR_SECONDS = 60.0

# pyobjc 是可选依赖：缺失时传感器整体降级为不可用，而不是让程序崩溃。
try:  # pragma: no cover - 依赖运行环境
    from AppKit import NSWorkspace  # type: ignore

    PYOBJC_AVAILABLE = True
except Exception:  # pragma: no cover
    NSWorkspace = None  # type: ignore
    PYOBJC_AVAILABLE = False


@dataclass
class _ActiveSegment:
    """当前正在累积的活动段。"""

    app: str
    bundle_id: str
    project: str | None
    start: datetime

    def duration(self, now: datetime) -> float:
        return max(0.0, (now - self.start).total_seconds())


class WindowSensor(Sensor):
    """前台应用活动采集。"""

    name = "window"

    def __init__(self, config: Config, privacy: PrivacyFilter, emit):
        super().__init__(emit)
        self.config = config
        self.privacy = privacy
        self._segment: _ActiveSegment | None = None
        self._last_checkpoint: datetime | None = None
        self._title_permission_warned = False
        # 挂起（锁屏 / idle / 暂停）期间不累计任何时长，恢复后重新开始计时。
        self._suspended = False
        self._sweep_logged = False
        self._last_poll_wall = datetime.now(timezone.utc)
        self._last_poll_mono = time.monotonic()

    def _run(self) -> None:
        if not PYOBJC_AVAILABLE:
            logger.error("未安装 pyobjc，Window 传感器不可用（pip install pyobjc-framework-Cocoa）")
            return

        poll = max(0.5, self.config.window_poll_seconds)
        checkpoint = max(5.0, self.config.window_checkpoint_seconds)

        while not self.should_stop():
            try:
                now = datetime.now(timezone.utc)
                mono = time.monotonic()
                locked = screen_is_locked()
                app, bundle_id, title = "", "", ""
                if not locked:
                    app, bundle_id, title = self._frontmost()
                self.poll_once(now, mono, poll, checkpoint, app, bundle_id, title, locked)
            except Exception:
                # 采集失败不能终止循环，否则会静默丢数据。
                logger.exception("Window 传感器采样失败")

            # 用可中断的等待代替 sleep，保证 stop() 能快速返回。
            if self._stop_event.wait(poll):
                break

        # 退出前关闭当前段，避免丢失已累积的时长。
        self._close_segment(datetime.now(timezone.utc))

    def poll_once(self, now: datetime, mono: float, poll_seconds: float, checkpoint_seconds: float,
                  app: str, bundle_id: str, title: str, locked: bool) -> None:
        """处理一次轮询。

        决策顺序：
        1. 睡眠检测——没锁屏直接合盖时，下一次轮询会看到「同一应用、时间过了 8 小时」，
           必须先把段切在睡前，否则整段睡眠被算成工作；
        2. 锁屏或系统伪应用——挂起计时；
        3. 其他情况——恢复计时并累计当前应用。
        """
        slept_until = self._sleep_boundary(now, mono, poll_seconds)
        if slept_until is not None:
            logger.debug("检测到系统睡眠，窗口计时段在 %s 处切断", slept_until)
            self.suspend(slept_until)

        # 第二层防御：即使 idle 传感器没通知到（线程异常、传感器被关闭），
        # 这里也会发现锁屏并挂起，绝不把锁屏时间算成工作。
        if locked:
            self.suspend(now)
            return

        if app and is_system_pseudo_app(app, bundle_id):
            # 系统伪应用：既不算工作，也要结束当前段。
            self.suspend(now)
            if not self._sweep_logged:
                logger.debug("检测到系统伪应用 %s，暂停窗口计时", app)
                self._sweep_logged = True
            return

        self.wake()
        if app:
            self._update_segment(app, bundle_id, title, now, checkpoint_seconds)

    def _sleep_boundary(self, now: datetime, mono: float, poll_seconds: float) -> datetime | None:
        """检测两次轮询之间是否发生过系统睡眠；返回睡着前的最后时刻。

        墙钟在睡眠期间照常前进，单调钟不会。返回值用于把活动段切在睡眠之前：
        只在锁屏时挂起并不够——没锁屏直接合盖睡眠时，下一次轮询会看到
        「同一个应用、时间过了 8 小时」，从而把它整段算成工作。
        """
        boundary = self._last_poll_wall
        wall_delta = (now - boundary).total_seconds()
        mono_delta = mono - self._last_poll_mono
        self._last_poll_wall = now
        self._last_poll_mono = mono

        gap = wall_delta - max(0.0, mono_delta)
        threshold = max(SUSPEND_GAP_FLOOR_SECONDS, 4 * max(1.0, poll_seconds))
        if gap >= threshold:
            return boundary + timedelta(seconds=poll_seconds)
        return None

    def _frontmost(self) -> tuple[str, str, str]:
        """返回前台应用的 (名称, bundle_id, 窗口标题)。

        标题在无权限时为空字符串，此时只记录应用名。
        """
        workspace = NSWorkspace.sharedWorkspace()
        app = workspace.frontmostApplication()
        if app is None:
            return "", "", ""

        name = str(app.localizedName() or "")
        bundle_id = str(app.bundleIdentifier() or "")
        title = self._window_title(app)

        # 黑名单应用完全跳过（其 idle 仍由 idle 传感器记录）。
        if self.config.is_blacklisted(name, bundle_id):
            return "", "", ""
        return name, bundle_id, title

    def _window_title(self, app) -> str:
        """尝试获取窗口标题。

        macOS 需要辅助功能权限才能读取其他应用的窗口标题。没有权限时返回空字符串，
        传感器自动降级为“只记录应用名”，这是可接受的降级路径。
        """
        try:
            from Quartz import (  # type: ignore
                CGWindowListCopyWindowInfo,
                kCGWindowListOptionOnScreenOnly,
                kCGNullWindowID,
            )

            pid = app.processIdentifier()
            windows = CGWindowListCopyWindowInfo(
                kCGWindowListOptionOnScreenOnly, kCGNullWindowID
            )
            for window in windows or []:
                if window.get("kCGWindowOwnerPID") == pid:
                    title = window.get("kCGWindowName")
                    if title:
                        return str(title)
        except Exception:
            if not self._title_permission_warned:
                logger.info("无法读取窗口标题，降级为只记录应用名（需要辅助功能权限）")
                self._title_permission_warned = True
        return ""

    def _update_segment(self, app: str, bundle_id: str, title: str,
                        now: datetime, checkpoint_seconds: float) -> None:
        """根据前台应用变化更新活动段。"""
        # 系统伪应用永远不进活动段；如果上一段还在累计，就在这里切断。
        if is_system_pseudo_app(app, bundle_id):
            self.suspend(now)
            return
        if self._suspended:
            # 挂起期间不累计；调用方（_run 或 Runtime）必须先 wake()。
            return
        project = self.privacy.project_from_title(title)

        if self._segment is None:
            self._segment = _ActiveSegment(app=app, bundle_id=bundle_id, project=project, start=now)
            self._last_checkpoint = now
            return

        current = self._segment
        if current.app != app or current.project != project:
            # 应用或项目变化：关闭上一段，开启新段。
            self._close_segment(now)
            self._segment = _ActiveSegment(app=app, bundle_id=bundle_id, project=project, start=now)
            self._last_checkpoint = now
            return

        # 同一应用持续使用：达到 checkpoint 间隔就落一条累积事件。
        if self._last_checkpoint and (now - self._last_checkpoint).total_seconds() >= checkpoint_seconds:
            duration = current.duration(now)
            if duration >= 1:
                # 重新计一段，避免重复计算已上报的时长。
                self._segment = _ActiveSegment(
                    app=app, bundle_id=bundle_id, project=project, start=now
                )
                self._last_checkpoint = now
                self._emit_window(current, duration, checkpoint=True)

    def _close_segment(self, now: datetime) -> None:
        """关闭当前活动段并产出事件。"""
        segment = self._segment
        self._segment = None
        self._last_checkpoint = None
        if segment is None:
            return
        duration = segment.duration(now)
        if duration < 1:
            # 不足 1 秒的切换不产生事件，减少噪声。
            return
        self._emit_window(segment, duration, checkpoint=False)

    def _emit_window(self, segment: _ActiveSegment, duration: float, checkpoint: bool) -> None:
        """构造并产出一条 window.activity 事件。"""
        event = Event.window_activity(
            event_id=new_ulid(),
            device_id=self.config.device_id,
            start=segment.start,
            duration_seconds=duration,
            app=segment.app,
            bundle_id=segment.bundle_id,
            # 注意：这里只传白名单命中的项目名，标题原文不进入事件。
            project=segment.project,
            checkpoint=checkpoint,
        )
        self.emit(event)

    def suspend(self, at: datetime) -> None:
        """挂起计时：结算当前段，之后不再累计任何时长。

        at 应传入「用户离开」的时刻（锁屏时刻，或最后一次输入时刻），
        而不是发现离开的时刻，否则会把离开前那几分钟算成工作。
        """
        self._close_segment(at)
        self._suspended = True

    def wake(self) -> None:
        """恢复计时。清掉挂起标记；真正的计时从下一次 _update_segment 开始。"""
        self._suspended = False

    @property
    def suspended(self) -> bool:
        """是否处于挂起状态（锁屏 / idle / 暂停）。"""
        return self._suspended

    def on_pause(self) -> None:
        """暂停时关闭当前段，避免把暂停期间算作工作时间。"""
        self.suspend(datetime.now(timezone.utc))

    def on_resume(self) -> None:
        """恢复采集。"""
        self.wake()

    def current_segment_info(self) -> dict[str, object] | None:
        """返回当前活动段信息，供 status 命令展示（不含标题）。"""
        if not self._segment:
            return None
        now = datetime.now(timezone.utc)
        return {
            "app": self._segment.app,
            "project": self._segment.project,
            "duration_seconds": int(self._segment.duration(now)),
            "start": self._segment.start.astimezone().isoformat(timespec="seconds"),
        }
