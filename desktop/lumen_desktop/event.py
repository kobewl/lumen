"""事件模型与字段 allowlist。

核心安全设计：上传字段通过 allowlist 逐项构造，绝不把原始对象整体序列化。
这样即使上层对象里混入了敏感字段（比如完整窗口标题），也不会被上传。
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

# V0.1 允许的事件类型，与服务端 events 包和 protocol schema 保持一致。
TYPE_WINDOW_ACTIVITY = "window.activity"
TYPE_IDLE_STATE = "idle.state"
TYPE_GIT_ACTIVITY = "git.activity"
# agent.task_summary 是专业 Agent（如 ZCode）主动汇报的任务摘要。
#
# 与原三个传感器事件的根本区别：那三个是"观察到什么"，这个是"Agent 自己报告
# 做完了什么"。它因此是回答"今天完成了什么"这类问题时唯一带结论的数据源，
# 而应用名 + 时长只能说明"用了什么"。
TYPE_AGENT_TASK_SUMMARY = "agent.task_summary"

ALLOWED_TYPES = {
    TYPE_WINDOW_ACTIVITY,
    TYPE_IDLE_STATE,
    TYPE_GIT_ACTIVITY,
    TYPE_AGENT_TASK_SUMMARY,
}

# agent.task_summary 的字段集版本。
TASK_SUMMARY_SCHEMA_VERSION = 1

# 任务摘要的隐私模式：只接受元数据级摘要。
#
# 这不是"目前还没做正文过滤"，而是协议层的硬约束——服务端只认这个值，
# 换任何别的写法都会被拒绝，因此不存在"先传正文以后再说"的路径。
PRIVACY_MODE_METADATA_ONLY = "metadata_only"

# 任务状态。unknown 是诚实的默认值：Agent 没给结论时不要替它下结论。
TASK_STATUSES = {"done", "partial", "blocked", "abandoned", "unknown"}

# 任务摘要各字段的长度上限。与服务端 events 包保持一致。
TASK_ID_MAX_LENGTH = 128
TASK_TITLE_MAX_LENGTH = 120
TASK_ITEM_MAX_LENGTH = 120
TASK_ITEMS_MAX = 8
SOURCE_AGENT_MAX_LENGTH = 64
SOURCE_SESSION_ID_MAX_LENGTH = 128

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

# context 的字段 allowlist。所有事件类型共用：context 只放"这条事件属于什么"。
ALLOWED_CONTEXT_KEYS = {"app", "bundle_id", "project", "repo"}

# 传感器事件的 data 字段 allowlist。
SENSOR_DATA_KEYS = {
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

# agent.task_summary 的 data 字段 allowlist。
TASK_SUMMARY_DATA_KEYS = {
    "schema_version",
    "task_id",
    "title",
    "status",
    "outcomes",
    "open_loops",
    "source_agent",
    "source_session_id",
    "privacy_mode",
}

# 全部事件类型允许的 data 字段并集。
#
# 仅供"这条 payload 有没有超出所有可能的字段"这类粗查使用。
# 精确校验必须用 ALLOWED_DATA_KEYS_BY_TYPE[event.type]：
# 并集里含有 title，用它校验传感器事件会漏掉窗口标题外泄。
ALLOWED_DATA_KEYS = SENSOR_DATA_KEYS | TASK_SUMMARY_DATA_KEYS

# 按事件类型分派的 data allowlist。
#
# 为什么要分派而不是用一张大表：title 只在 agent.task_summary 里是合法字段
# （任务标题），在任何传感器事件里都意味着窗口标题外泄。共用一个集合会让
# "允许任务标题"顺带把"允许窗口标题"也放进来，这是不能接受的放宽。
ALLOWED_DATA_KEYS_BY_TYPE = {
    TYPE_WINDOW_ACTIVITY: SENSOR_DATA_KEYS,
    TYPE_IDLE_STATE: SENSOR_DATA_KEYS,
    TYPE_GIT_ACTIVITY: SENSOR_DATA_KEYS,
    TYPE_AGENT_TASK_SUMMARY: TASK_SUMMARY_DATA_KEYS,
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
    "absolute_path": "不上传绝对路径",
    "path": "不上传绝对路径",
    "remote_url": "不上传 Git remote URL",
    "username": "不上传用户名",
    "token": "不上传任何凭证",
}

# 禁止出现在 context 里的字段。
#
# 单独一张表的原因：data.title 对任务摘要合法，但 context.title 永远是窗口标题，
# 任何事件类型下都不允许。把两者混在一起会导致要么误放窗口标题、要么误禁任务标题。
FORBIDDEN_CONTEXT_KEYS = {
    "title": "窗口标题默认不上传",
    "window_title": "窗口标题默认不上传",
}

# 事件状态机。
STATUS_CREATED = "created"
STATUS_READY = "ready_to_sync"
STATUS_SYNCED = "synced"
STATUS_REJECTED = "rejected"


# task_id 允许的字符集：字母数字与 . _ : -
#
# 排除空格、斜杠和引号，避免任务 ID 本身成为注入载体或路径片段。
TASK_ID_PATTERN = re.compile(r"^[A-Za-z0-9._:-]+$")


class PrivacyError(ValueError):
    """当事件试图携带禁止字段时抛出。"""


def _clean_text(value: str, field: str, limit: int) -> str:
    """裁剪单行文本并做长度校验。

    换行会被压成空格：任务标题与结果条目都应是单行，多行内容意味着
    调用方想把整段文本塞进来（例如对话原文），这是在滥用元数据字段。
    """
    text = " ".join(str(value).split())
    if len(text) > limit:
        raise PrivacyError(f"{field} 超过 {limit} 字上限（当前 {len(text)} 字）")
    return text


def _clean_token(value: str, field: str, limit: int) -> str:
    """校验标识类字段：无空白、字符集受限、长度受限。"""
    text = str(value).strip()
    if not text:
        raise PrivacyError(f"{field} 不能为空")
    if len(text) > limit:
        raise PrivacyError(f"{field} 超过 {limit} 字符上限")
    if not TASK_ID_PATTERN.match(text):
        raise PrivacyError(f"{field} 只允许字母、数字与 . _ : -")
    return text


def _clean_items(items: list[str] | None, field: str) -> list[str]:
    """裁剪条目数组：逐条清空、去重、限长、限量。"""
    if not items:
        return []
    out: list[str] = []
    for raw in items:
        text = _clean_text(raw, field, TASK_ITEM_MAX_LENGTH)
        if text and text not in out:
            out.append(text)
        if len(out) >= TASK_ITEMS_MAX:
            break
    return out


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
        self._check_keys(self.data, self.allowed_data_keys(), "data")

    def allowed_data_keys(self) -> set[str]:
        """返回该事件类型允许的 data 字段集合。"""
        return ALLOWED_DATA_KEYS_BY_TYPE[self.type]

    @staticmethod
    def _check_keys(payload: dict[str, Any], allowed: set[str], scope: str) -> None:
        """按 allowlist 校验字段名；命中禁用字段立即报错。"""
        for key in payload:
            if scope == "context" and key in FORBIDDEN_CONTEXT_KEYS:
                raise PrivacyError(f"{scope}.{key} 被禁止: {FORBIDDEN_CONTEXT_KEYS[key]}")
            if key in FORBIDDEN_KEYS:
                raise PrivacyError(f"{scope}.{key} 被禁止: {FORBIDDEN_KEYS[key]}")
            if key not in allowed:
                raise PrivacyError(f"{scope} 存在未声明字段: {key}")

    def to_payload(self) -> dict[str, Any]:
        """构造用于上传的 JSON 结构（只包含 allowlist 字段）。"""
        allowed_data = self.allowed_data_keys()
        return {
            "id": self.id,
            "device_id": self.device_id,
            "type": self.type,
            "timestamp": utc_rfc3339(self.timestamp),
            "privacy": self.privacy,
            "context": {k: v for k, v in self.context.items() if k in ALLOWED_CONTEXT_KEYS},
            "data": {k: v for k, v in self.data.items() if k in allowed_data},
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
    def agent_task_summary(
        cls, event_id: str, device_id: str, occurred_at: datetime, task_id: str,
        title: str, source_agent: str, status: str = "unknown",
        outcomes: list[str] | None = None, open_loops: list[str] | None = None,
        source_session_id: str = "", project: str | None = None,
        app: str = "ZCode",
    ) -> "Event":
        """Agent 任务摘要事件。

        只接受元数据级摘要：标题、状态、已产出结果、未完成事项。
        完整对话、终端输出、代码、diff 与凭证都不是这个接口的参数——
        它们在字段 allowlist 之外，传进来会直接抛 PrivacyError，
        而不是"传了但被过滤掉"。这样泄露在本地就是不可能的，不依赖服务端兜底。

        occurred_at 写入事件顶层的 timestamp：一条事件只有一个权威时间点，
        再在 data 里放一份 occurred_at 会让两者可能不一致。
        """
        task_id = _clean_token(task_id, "task_id", TASK_ID_MAX_LENGTH)
        source_agent = _clean_token(source_agent, "source_agent", SOURCE_AGENT_MAX_LENGTH)
        if status not in TASK_STATUSES:
            raise PrivacyError(f"非法任务状态: {status}")
        if not title or not title.strip():
            raise PrivacyError("任务标题不能为空")

        context: dict[str, Any] = {"app": app}
        if project:
            context["project"] = project

        data: dict[str, Any] = {
            "schema_version": TASK_SUMMARY_SCHEMA_VERSION,
            "task_id": task_id,
            "title": _clean_text(title, "title", TASK_TITLE_MAX_LENGTH),
            "status": status,
            "outcomes": _clean_items(outcomes, "outcomes"),
            "open_loops": _clean_items(open_loops, "open_loops"),
            "source_agent": source_agent,
            "privacy_mode": PRIVACY_MODE_METADATA_ONLY,
        }
        if source_session_id:
            data["source_session_id"] = _clean_text(
                source_session_id, "source_session_id", SOURCE_SESSION_ID_MAX_LENGTH
            )

        return cls(
            id=event_id, device_id=device_id, type=TYPE_AGENT_TASK_SUMMARY,
            timestamp=occurred_at, privacy=PRIVACY_P1, context=context, data=data,
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
