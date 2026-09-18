"""传感器基类。

约束：
- 传感器只产生事件并交给回调，绝不直接访问网络；
- 每个传感器都必须支持 stop()，且不得留下无界任务或队列；
- 采集暂停（paused）时不产生新的工作事件。
"""

from __future__ import annotations

import logging
import threading
from abc import ABC, abstractmethod
from typing import Callable

from ..event import Event

logger = logging.getLogger(__name__)

EventCallback = Callable[[Event], None]


class Sensor(ABC):
    """所有传感器的公共接口。"""

    name = "sensor"

    def __init__(self, emit: EventCallback):
        self._emit = emit
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None
        # 暂停开关由外部（配置/CLI）控制；暂停时传感器继续运行但不产出事件。
        self._paused = threading.Event()

    def start(self) -> None:
        """启动传感器线程。"""
        if self._thread and self._thread.is_alive():
            return
        self._stop_event.clear()
        self._thread = threading.Thread(target=self._run, name=f"lumen-{self.name}", daemon=True)
        self._thread.start()
        logger.info("传感器 %s 已启动", self.name)

    def stop(self, timeout: float = 5.0) -> None:
        """停止传感器并等待线程退出。"""
        self._stop_event.set()
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=timeout)
            if self._thread.is_alive():
                logger.warning("传感器 %s 未能在 %s 秒内退出", self.name, timeout)
        logger.info("传感器 %s 已停止", self.name)

    def pause(self) -> None:
        """暂停采集：不再产生新的工作事件。"""
        self._paused.set()
        self.on_pause()

    def resume(self) -> None:
        """恢复采集。"""
        self._paused.clear()
        self.on_resume()

    @property
    def paused(self) -> bool:
        return self._paused.is_set()

    def should_stop(self) -> bool:
        return self._stop_event.is_set()

    def emit(self, event: Event) -> None:
        """产出事件；暂停时直接丢弃，避免产生“暂停期间的工作事件”。"""
        if self._paused.is_set():
            return
        try:
            self._emit(event)
        except Exception:  # 单个事件失败不应终止传感器
            logger.exception("事件回调失败, event_id=%s type=%s", event.id, event.type)

    # 子类钩子
    def on_pause(self) -> None:
        """暂停时的清理动作，例如关闭当前活动段。"""

    def on_resume(self) -> None:
        """恢复时的重置动作。"""

    @abstractmethod
    def _run(self) -> None:
        """传感器主循环，需自行处理 stop 与异常。"""
