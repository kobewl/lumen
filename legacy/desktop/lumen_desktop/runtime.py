"""采集端运行时：组装传感器、存储与同步循环。

职责划分：
- 传感器 → 事件 → 本地 SQLite（离线缓冲）；
- 同步线程按间隔或阈值触发上传；
- 所有异常都在这里被捕获并记录，保证长时间运行不崩溃。
"""

from __future__ import annotations

import logging
import signal
import threading
import time
from datetime import datetime, timezone
from typing import Any

from .config import Config
from .event import Event
from .keychain import load_token
from .privacy import PrivacyFilter
from .sensors import GitSensor, IdleSensor, WindowSensor
from .storage import Storage
from .sync import SyncClient

logger = logging.getLogger(__name__)


class Runtime:
    """采集端主运行时。"""

    def __init__(self, config: Config, storage: Storage, token: str | None = None):
        self.config = config
        self.storage = storage
        self.token = token if token is not None else load_token()
        self.privacy = PrivacyFilter(config.project_keywords)
        self.sync_client = SyncClient(config, storage, self.token or "", self.privacy)

        self._stop = threading.Event()
        self._sync_thread: threading.Thread | None = None
        self._pause_flag = threading.Event()
        if config.paused:
            self._pause_flag.set()

        self.window_sensor = WindowSensor(config, self.privacy, self._handle_event)
        self.idle_sensor = IdleSensor(config, self._handle_event, on_state_change=self._on_idle_state)
        self.git_sensor = GitSensor(config, self.privacy, self._handle_event)
        self._sensors = [self.window_sensor, self.idle_sensor, self.git_sensor]

        # 采集统计，供 status 命令展示。
        self._stats = {"emitted": 0, "stored": 0, "dropped_duplicate": 0, "paused_skipped": 0}
        self._stats_lock = threading.Lock()

    # ---- 生命周期 ----

    def start(self) -> None:
        """启动传感器与同步循环。"""
        if self._pause_flag.is_set():
            for sensor in self._sensors:
                sensor.pause()

        for sensor, enabled in (
            (self.window_sensor, self.config.enable_window_sensor),
            (self.idle_sensor, self.config.enable_idle_sensor),
            (self.git_sensor, self.config.enable_git_sensor),
        ):
            if enabled:
                sensor.start()
            else:
                logger.info("传感器 %s 已在配置中关闭", sensor.name)

        self._sync_thread = threading.Thread(target=self._sync_loop, name="lumen-sync", daemon=True)
        self._sync_thread.start()

        # 启动时立即尝试一次同步，把上次退出时未传完的数据送出去。
        threading.Thread(target=self._safe_sync, args=(True,), name="lumen-sync-initial", daemon=True).start()
        logger.info("Lumen 采集端已启动")

    def stop(self) -> None:
        """停止全部线程并做一次收尾处理。"""
        logger.info("正在停止 Lumen 采集端")
        self._stop.set()
        for sensor in self._sensors:
            try:
                sensor.stop()
            except Exception:
                logger.exception("停止传感器 %s 失败", sensor.name)

        if self._sync_thread and self._sync_thread.is_alive():
            self._sync_thread.join(timeout=10)

        # 退出前尽力同步一次，并做 WAL checkpoint。
        self._safe_sync(force=True)
        try:
            self.storage.checkpoint()
        except Exception:
            logger.exception("退出前 checkpoint 失败")
        logger.info("Lumen 采集端已停止")

    def run_forever(self) -> None:
        """阻塞运行，直到收到 SIGINT/SIGTERM。

        另外响应两个自定义信号，让 `lumen-desktop pause/resume` 可以在不重启
        进程的情况下切换采集状态：
          SIGUSR1 → 暂停采集
          SIGUSR2 → 恢复采集
        """
        self.start()

        stop_event = threading.Event()

        def handle_stop(signum, _frame):
            logger.info("收到信号 %s，准备退出", signum)
            stop_event.set()

        def handle_pause(_signum, _frame):
            self.pause()

        def handle_resume(_signum, _frame):
            self.resume()

        handlers = {
            signal.SIGINT: handle_stop,
            signal.SIGTERM: handle_stop,
            signal.SIGUSR1: handle_pause,
            signal.SIGUSR2: handle_resume,
        }
        for sig, handler in handlers.items():
            try:
                signal.signal(sig, handler)
            except (ValueError, AttributeError, OSError):
                # 非主线程或平台不支持时忽略，主流程仍可用。
                pass

        try:
            while not stop_event.is_set() and not self._stop.is_set():
                stop_event.wait(1.0)
        except KeyboardInterrupt:
            pass
        finally:
            self.stop()

    # ---- 事件处理 ----

    def _handle_event(self, event: Event) -> None:
        """把事件写入本地库，并在需要时触发立即同步。"""
        try:
            stored = self.storage.store_event(event)
        except Exception:
            logger.exception("写入事件失败, event_id=%s", event.id)
            return

        with self._stats_lock:
            self._stats["emitted"] += 1
            if stored:
                self._stats["stored"] += 1
            else:
                self._stats["dropped_duplicate"] += 1

        # 队列达到阈值时立即同步，而不是等到下一个间隔。
        if stored and self._should_sync_now():
            threading.Thread(target=self._safe_sync, args=(True,), daemon=True).start()

    def _should_sync_now(self) -> bool:
        """判断是否达到“立即同步”的阈值：100 条或 512KB。"""
        try:
            counts = self.storage.counts()
            if counts.get("queued", 0) >= self.config.sync_batch_max_events:
                return True
            size = self.storage.queue_size_bytes()
            if size >= self.config.sync_batch_max_bytes:
                return True
            # 软上限告警（不阻塞采集）。
            if size >= self.config.queue_soft_limit_mb * 1024 * 1024:
                logger.warning("本地未同步数据已达软上限 %d MB", self.config.queue_soft_limit_mb)
        except Exception:
            logger.exception("检查同步阈值失败")
        return False

    def _on_idle_state(self, state: str, started_at: datetime) -> None:
        """idle 状态变化时同步窗口传感器的计时开关。

        关键点：切断点用状态区间的**起点**，也就是用户真正离开的时刻，
        而不是发现状态变化的时刻。否则 idle 判定延迟（最多一个轮询周期）
        会把离开前的时间算成工作。
        """
        try:
            if state == "active":
                self.window_sensor.wake()
            else:
                self.window_sensor.suspend(started_at)
        except Exception:
            logger.exception("处理 idle 状态变化失败")

    # ---- 同步循环 ----

    def _sync_loop(self) -> None:
        interval = max(30.0, self.config.sync_interval_seconds)
        while not self._stop.wait(interval):
            self._safe_sync(force=False)

    def _safe_sync(self, force: bool = False) -> None:
        """执行一次同步，吞掉所有异常（长时间运行优先保证不崩溃）。"""
        try:
            result = self.sync_client.sync_once(force=force)
            if result.skipped:
                # 无事可做（队列为空）时，「已安排重试」已经没有对应待办，
                # 必须清掉，否则一次网络抖动会让 status 永久显示旧错误。
                # 真实阻塞（未注册、硬上限、事件被拒）则要保留提醒。
                logger.debug("跳过同步: %s", result.message)
                if result.blocked:
                    self.storage.set_meta("last_sync_error", result.message)
                else:
                    self.storage.set_meta("last_sync_error", None)
                return
            if result.rejected:
                logger.warning("同步结果: %s", result.message)
                for detail in result.rejected_details[:5]:
                    logger.warning("被拒绝的事件 %s: %s", detail["event_id"], detail["code"])
            else:
                logger.info("同步结果: %s", result.message)

            # last_sync_error 必须反映当前状况：同步恢复正常后要清除，
            # 否则一次网络抖动会让 status 永远显示「上次错误」。
            # 被拒事件是数据没能上报，属于需要用户知道的真实问题，照常记录。
            if result.retried or result.rejected:
                self.storage.set_meta("last_sync_error", result.message)
            else:
                self.storage.set_meta("last_sync_error", None)
        except Exception:
            logger.exception("同步过程出现未预期异常")

    # ---- 控制接口（CLI 使用） ----

    def pause(self) -> None:
        """暂停采集：不产生新的工作事件，但保持同步。"""
        self._pause_flag.set()
        self.config.paused = True
        for sensor in self._sensors:
            sensor.pause()
        logger.info("采集已暂停")

    def resume(self) -> None:
        """恢复采集。"""
        self._pause_flag.clear()
        self.config.paused = False
        for sensor in self._sensors:
            sensor.resume()
        logger.info("采集已恢复")

    def sync_now(self) -> dict[str, Any]:
        """立即同步一次，返回结果摘要。"""
        result = self.sync_client.sync_once(force=True)
        return {
            "attempted": result.attempted,
            "accepted": result.accepted,
            "duplicate": result.duplicate,
            "rejected": result.rejected,
            "retried": result.retried,
            "skipped": result.skipped,
            "message": result.message,
        }

    def status(self) -> dict[str, Any]:
        """返回运行状态，供 CLI status 展示。"""
        counts = self.storage.counts()
        with self._stats_lock:
            stats = dict(self._stats)
        return {
            "paused": self._pause_flag.is_set(),
            "device_id": self.config.device_id,
            "server_url": self.config.server_url,
            "registered": bool(self.token),
            "counts": counts,
            "queue_mb": round(self.storage.queue_size_bytes() / 1024 / 1024, 2),
            "last_sync_at": self.storage.get_meta("last_sync_at"),
            "current_window": self.window_sensor.current_segment_info(),
            "window_suspended": self.window_sensor.suspended,
            "idle_state": self.idle_sensor.current_state,
            "git_repos": self.git_sensor.repos(),
            "sensors": {
                "window": self.config.enable_window_sensor,
                "idle": self.config.enable_idle_sensor,
                "git": self.config.enable_git_sensor,
            },
            "session_stats": stats,
            "now": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "uptime_check_seconds": int(time.monotonic()),
        }
