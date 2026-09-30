"""传感器包。

约束：任何传感器都不得直接访问网络；上传字段一律通过 event.Event 的
allowlist 构造，不能把原始对象整体序列化。
"""

from .base import EventCallback, Sensor
from .git import GitSensor
from .idle import IdleSensor
from .window import WindowSensor

__all__ = ["Sensor", "EventCallback", "WindowSensor", "IdleSensor", "GitSensor"]
