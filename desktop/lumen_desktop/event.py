"""事件模型与字段 allowlist。

核心安全设计：上传字段通过 allowlist 逐项构造，绝不把原始对象整体序列化。
这样即使上层对象里混入了敏感字段（比如完整窗口标题），也不会被上传。
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

# V0.1 允许的事件类型，与服务端 events 包和 protocol schema 保持一致。
TYPE_WINDOW_ACTIVITY = "window.activity"
TYPE_IDLE_STATE = "idle.state"
TYPE_GIT_ACTIVITY = "git.activity"

ALLOWED_TYPES = {TYPE_WINDOW_ACTIVITY, TYPE_IDLE_STATE, TYPE_GIT_ACTIVITY}

PRIVACY_P0 = "P0"
PRIVACY_P1 = "P1"

# idle.state 的区间语义版本。
#
# 语义（采集端与服务端必须一致）：
#   timestamp             = 状态区间的开始时刻
#   data.duration_seconds = 区间时长；缺省表示区间仍在进行中
#
# 旧实现写的是「上一段状态 + 变化时刻」，服务端因此无法正确切断 Session
# （真机数据里出现了 45094 秒的 active 区间）。历史事件按旧语义解释：
# timestamp 是区间结束时刻，服务端靠 schema_version 缺失来识别。
IDLE_SCHEMA_VERSION = 2

# context 与 data 的字段 allowlist。
ALLOWED_CONTEXT_KEYS = {"app", "bundle_id", "project", "repo"}
ALLOWED_DATA_KEYS = {
    "duration_seconds",
    "checkpoint",
    "state",
    "schema_version",
    "branch",
    "head_commit",
    "commit_message",
    "changed_files_count",
    "kind",
}

# 明确禁止的字段：命中即抛异常，防止未来误加采集。
FORBIDDEN_KEYS = {
    "clipboard": "V0.1 不采集剪贴板",
    "clipboard_text": "V0.1 不采集剪贴板",
    "screen": "V0.1 不采集屏幕",
    "screenshot": "V0.1 不采集截图",
    "source_code": "V0.1 不采集源代码",
    "code": "V0.1 不采集源代码",
    "diff": "V0.1 不采集 diff",
    "patch": "V0.1 不采集 diff",
    "terminal_output": "V0.1 不采集终端输出",
    "file_content": "V0.1 不采集文件正文",
    "window_title": "窗口标题默认不上传",
    "title": "窗口标题默认不上传",
    "absolute_path": "不上传绝对路径",
    "path": "不上传绝对路径",
    "remote_url": "不上传 Git remote URL",
    "username": "不上传用户名",
    "token": "不上传任何凭证",
}

# 事件状态机。
STATUS_CREATED = "created"
STATUS_READY = "ready_to_sync"
STATUS_SYNCED = "synced"
STATUS_REJECTED = "rejected"


class PrivacyError(ValueError):
    """当事件试图携带禁止字段时抛出。"""


def utc_rfc3339(ts: datetime) -> str:
    """转换成协议要求的 UTC RFC3339 字符串（以 Z 结尾）。"""
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    return ts.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_rfc3339(value: str) -> datetime:
    """解析 RFC3339 时间字符串为 UTC datetime。"""
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    dt = datetime.fromisoformat(text)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


@dataclass
class Event:
    """一条待上传的事件。"""

    id: str
    device_id: str
    type: str
    timestamp: datetime
    privacy: str
    context: dict[str, Any] = field(default_factory=dict)
    data: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.type not in ALLOWED_TYPES:
            raise PrivacyError(f"未知事件类型: {self.type}")
        if self.privacy not in (PRIVACY_P0, PRIVACY_P1):
            raise PrivacyError(f"非法隐私等级: {self.privacy}")
        self._check_keys(self.context, ALLOWED_CONTEXT_KEYS, "context")
        self._check_keys(self.data, ALLOWED_DATA_KEYS, "data")

    @staticmethod
    def _check_keys(payload: dict[str, Any], allowed: set[str], scope: str) -> None:
        """按 allowlist 校验字段名；命中禁用字段立即报错。"""
        for key in payload:
            if key in FORBIDDEN_KEYS:
                raise PrivacyError(f"{scope}.{key} 被禁止: {FORBIDDEN_KEYS[key]}")
            if key not in allowed:
                raise PrivacyError(f"{scope} 存在未声明字段: {key}")

    def to_payload(self) -> dict[str, Any]:
        """构造用于上传的 JSON 结构（只包含 allowlist 字段）。"""
        return {
            "id": self.id,
            "device_id": self.device_id,
            "type": self.type,
            "timestamp": utc_rfc3339(self.timestamp),
            "privacy": self.privacy,
            "context": {k: v for k, v in self.context.items() if k in ALLOWED_CONTEXT_KEYS},
            "data": {k: v for k, v in self.data.items() if k in ALLOWED_DATA_KEYS},
        }

    def to_json(self) -> str:
        """序列化为 JSON 字符串。"""
        return json.dumps(self.to_payload(), ensure_ascii=False)

    def size_bytes(self) -> int:
        """事件序列化后的字节数，用于批量大小控制。"""
        return len(self.to_json().encode("utf-8"))

    # ---- 工厂方法：把各传感器的原始观察转成最小化事件 ----

    @classmethod
    def window_activity(
        cls, event_id: str, device_id: str, start: datetime, duration_seconds: float,
        app: str, bundle_id: str = "", project: str | None = None, checkpoint: bool = False,
    ) -> "Event":
        """窗口活动事件。只包含应用名、时长和（可选的）白名单项目名。"""
        context: dict[str, Any] = {"app": app}
        if bundle_id:
            context["bundle_id"] = bundle_id
        if project:
            context["project"] = project
        data: dict[str, Any] = {"duration_seconds": max(0, int(duration_seconds))}
        if checkpoint:
            data["checkpoint"] = True
        return cls(
            id=event_id, device_id=device_id, type=TYPE_WINDOW_ACTIVITY,
            timestamp=start, privacy=PRIVACY_P0, context=context, data=data,
        )

    @classmethod
    def idle_state(
        cls, event_id: str, device_id: str, start: datetime, state: str, duration_seconds: float = 0,
    ) -> "Event":
        """空闲状态区间事件。

        timestamp 是区间**开始**时刻；duration_seconds 缺省表示区间还没结束。
        """
        if state not in ("active", "idle", "locked"):
            raise PrivacyError(f"非法 idle 状态: {state}")
        data: dict[str, Any] = {"state": state, "schema_version": IDLE_SCHEMA_VERSION}
        if duration_seconds > 0:
            data["duration_seconds"] = int(duration_seconds)
        return cls(
            id=event_id, device_id=device_id, type=TYPE_IDLE_STATE,
            timestamp=start, privacy=PRIVACY_P0, context={}, data=data,
        )

    @classmethod
    def git_activity(
        cls, event_id: str, device_id: str, when: datetime, repo: str, kind: str = "commit",
        branch: str = "", head_commit: str = "", commit_message: str = "",
        changed_files_count: int = 0, project: str | None = None,
    ) -> "Event":
        """Git 活动事件。只上报元数据，不含 diff、文件正文和 remote URL。"""
        if kind not in ("commit", "workspace"):
            raise PrivacyError(f"非法 Git 事件类型: {kind}")
        context: dict[str, Any] = {"repo": repo}
        if project:
            context["project"] = project
        data: dict[str, Any] = {"kind": kind}
        if branch:
            data["branch"] = branch
        if head_commit:
            data["head_commit"] = head_commit
        if commit_message:
            # 只保留首行，且限制长度，避免把长正文带上服务器。
            first_line = commit_message.splitlines()[0] if commit_message else ""
            data["commit_message"] = first_line[:200]
        if changed_files_count > 0:
            data["changed_files_count"] = int(changed_files_count)
        return cls(
            id=event_id, device_id=device_id, type=TYPE_GIT_ACTIVITY,
            timestamp=when, privacy=PRIVACY_P1, context=context, data=data,
        )
