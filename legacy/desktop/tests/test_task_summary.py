"""agent.task_summary 事件的隐私边界与本地入口测试。

这一组测试保护的核心是**边界本身**：任务摘要允许带"结论"（标题、状态、
结果），但绝不允许把完整对话、终端输出、代码或 diff 带进来。
边界一旦松动，后续所有基于它的信任都会出问题，所以这里逐条钉死。
"""

from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from lumen_desktop.event import (  # noqa: E402
    ALLOWED_DATA_KEYS_BY_TYPE,
    TASK_ITEMS_MAX,
    TASK_SUMMARY_SCHEMA_VERSION,
    TYPE_AGENT_TASK_SUMMARY,
    TYPE_GIT_ACTIVITY,
    TYPE_WINDOW_ACTIVITY,
    Event,
    PrivacyError,
)
from lumen_desktop.privacy import PrivacyFilter  # noqa: E402

EVENT_ID = "01J9Z4QK7M3F8N2P5R7T9V1X3E"
DEVICE_ID = "desktop-mac-01"
WHEN = datetime(2026, 9, 17, 9, 42, 0, tzinfo=timezone.utc)


def make_task_event(**overrides) -> Event:
    kwargs = {
        "event_id": EVENT_ID,
        "device_id": DEVICE_ID,
        "occurred_at": WHEN,
        "task_id": "zcode-2026-09-17-001",
        "title": "接通任务摘要链路",
        "source_agent": "zcode-cli",
        "status": "done",
        "outcomes": ["新增事件类型"],
        "open_loops": ["尚未接入真实上报"],
        "source_session_id": "sess-9f3c1d8b",
        "project": "lumen",
    }
    kwargs.update(overrides)
    return Event.agent_task_summary(**kwargs)


def test_task_summary_payload_has_expected_shape() -> None:
    """正常任务摘要的字段与协议一致。"""
    payload = make_task_event().to_payload()

    assert payload["type"] == TYPE_AGENT_TASK_SUMMARY
    assert payload["privacy"] == "P1"
    assert payload["context"] == {"app": "ZCode", "project": "lumen"}

    data = payload["data"]
    assert data["schema_version"] == TASK_SUMMARY_SCHEMA_VERSION
    assert data["privacy_mode"] == "metadata_only"
    assert data["task_id"] == "zcode-2026-09-17-001"
    assert data["status"] == "done"
    assert data["outcomes"] == ["新增事件类型"]
    assert data["open_loops"] == ["尚未接入真实上报"]
    assert data["source_agent"] == "zcode-cli"
    # 来源会话 ID 用于追溯，但不应出现在用户可见文本里（由服务端与
    # assistant 层过滤），这里只确认它确实在事件里。
    assert data["source_session_id"] == "sess-9f3c1d8b"


def test_timestamp_is_the_authoritative_time() -> None:
    """occurred_at 写在顶层 timestamp，不在 data 里重复。

    一条事件只有一个权威时间点：两处都放会让它们可能不一致，
    而这种不一致在跨时区或补报时很难发现。
    """
    payload = make_task_event().to_payload()

    assert payload["timestamp"] == "2026-09-17T09:42:00Z"
    assert "occurred_at" not in payload["data"]


def test_title_is_allowed_for_task_summary_but_not_sensors() -> None:
    """title 只在任务摘要里合法 —— 传感器事件里它意味着窗口标题外泄。"""
    # 1) 任务摘要接受 title。
    assert make_task_event().to_payload()["data"]["title"] == "接通任务摘要链路"

    # 2) 传感器事件的 data.title 被拒绝。
    with pytest.raises(PrivacyError):
        Event(
            id=EVENT_ID, device_id=DEVICE_ID, type=TYPE_WINDOW_ACTIVITY,
            timestamp=WHEN, privacy="P0",
            context={"app": "Safari"}, data={"duration_seconds": 60, "title": "银行登录"},
        )

    # 3) 任何事件类型的 context.title 都被拒绝（那永远是窗口标题）。
    for event_type, data in (
        (TYPE_WINDOW_ACTIVITY, {"duration_seconds": 60}),
        (TYPE_GIT_ACTIVITY, {"kind": "commit"}),
        (TYPE_AGENT_TASK_SUMMARY, {"task_id": "t", "title": "任务标题",
                                   "status": "done", "source_agent": "a",
                                   "privacy_mode": "metadata_only"}),
    ):
        context = {"repo": "lumen"} if event_type == TYPE_GIT_ACTIVITY else {"app": "X"}
        context["title"] = "银行登录页面"
        with pytest.raises(PrivacyError):
            Event(
                id=EVENT_ID, device_id=DEVICE_ID, type=event_type,
                timestamp=WHEN, privacy="P1" if event_type != TYPE_WINDOW_ACTIVITY else "P0",
                context=context, data=data,
            )


@pytest.mark.parametrize("key,value", [
    ("conversation", [{"role": "user", "content": "帮我改代码"}]),
    ("messages", [{"role": "assistant", "content": "好的"}]),
    ("terminal_output", "npm ERR! code ELIFECYCLE"),
    ("code", "def main(): pass"),
    ("source_code", "print(1)"),
    ("diff", "--- a/x.py\n+++ b/x.py"),
    ("patch", "@@ -1,3 +1,4 @@"),
    ("token", "sk-abcdef123456"),
    ("clipboard", "copied"),
    ("screenshot", "base64..."),
])
def test_task_summary_rejects_content_payloads(key: str, value: object) -> None:
    """完整对话、终端输出、代码、diff、凭证在本地就被拒绝。

    注意这不是"传了会被过滤掉"：字段根本不在 allowlist 里，
    因此数据在离开这台机器之前就已经被拒绝。
    """
    event = make_task_event()
    event.data[key] = value
    with pytest.raises(PrivacyError):
        Event(
            id=event.id, device_id=event.device_id, type=event.type,
            timestamp=event.timestamp, privacy=event.privacy,
            context=event.context, data=event.data,
        )


def test_task_summary_rejects_oversized_title() -> None:
    """标题超长说明调用方想塞正文，直接拒绝而不是截断。"""
    with pytest.raises(PrivacyError):
        make_task_event(title="字" * 200)


def test_task_summary_truncates_and_dedupes_items() -> None:
    """条目会去重并限量，避免一次汇报整份清单。"""
    outcomes = [f"结果 {i}" for i in range(TASK_ITEMS_MAX + 5)]
    payload = make_task_event(outcomes=outcomes).to_payload()
    assert len(payload["data"]["outcomes"]) == TASK_ITEMS_MAX

    dup = make_task_event(outcomes=["同一个", "同一个", "另一个"]).to_payload()
    assert dup["data"]["outcomes"] == ["同一个", "另一个"]


def test_task_summary_rejects_multiline_title() -> None:
    """多行标题会被压成一行 —— 多行意味着想塞整段文本。"""
    payload = make_task_event(title="第一行\n第二行").to_payload()
    assert "\n" not in payload["data"]["title"]
    assert payload["data"]["title"] == "第一行 第二行"


def test_task_summary_rejects_bad_task_id() -> None:
    """task_id 限制字符集，避免 ID 本身成为载荷或路径片段。"""
    for bad in ("task id with spaces", "../../etc/passwd", "a/b", 'q"uote', ""):
        with pytest.raises(PrivacyError):
            make_task_event(task_id=bad)


def test_task_summary_rejects_unknown_status() -> None:
    """状态必须在白名单内，不接受 Agent 自造的状态值。"""
    with pytest.raises(PrivacyError):
        make_task_event(status="in_progress")


def test_data_allowlist_is_per_event_type() -> None:
    """data 的 allowlist 按事件类型分派，不是共用一张大表。

    共用会让"允许任务标题"顺带把"允许窗口标题"也放进来。
    """
    task_keys = ALLOWED_DATA_KEYS_BY_TYPE[TYPE_AGENT_TASK_SUMMARY]
    window_keys = ALLOWED_DATA_KEYS_BY_TYPE[TYPE_WINDOW_ACTIVITY]

    assert "title" in task_keys
    assert "title" not in window_keys
    # 任务摘要不该复用传感器字段。
    assert "duration_seconds" not in task_keys
    assert "commit_message" not in task_keys


def test_privacy_filter_rejects_paths_in_task_fields() -> None:
    """上传前的内容检查覆盖任务摘要的字段，包括条目数组。"""
    privacy = PrivacyFilter()

    ok = make_task_event().to_payload()
    assert privacy.validate_payload(ok) == []

    # 标题里夹带绝对路径。
    bad_title = make_task_event(title="改动 /Users/liang/x.go").to_payload()
    assert privacy.validate_payload(bad_title) != []

    # 结果条目里夹带 URL —— 条目是数组，必须逐项检查。
    bad_item = make_task_event(outcomes=["见 https://github.com/x/y"]).to_payload()
    assert privacy.validate_payload(bad_item) != []

    # 未完成事项里夹带凭证。
    bad_secret = make_task_event(open_loops=["用 sk-abcdef123456 重试"]).to_payload()
    assert privacy.validate_payload(bad_secret) != []


def test_privacy_filter_title_exception_is_precise() -> None:
    """data.title 的例外必须精确到事件类型，不能放宽到传感器事件。"""
    privacy = PrivacyFilter()

    task_payload = make_task_event().to_payload()
    assert privacy.validate_payload(task_payload) == []

    window_payload = {
        "type": TYPE_WINDOW_ACTIVITY,
        "context": {"app": "Safari"},
        "data": {"duration_seconds": 60, "title": "银行登录"},
    }
    problems = privacy.validate_payload(window_payload)
    assert problems != [], "传感器事件的 data.title 必须被判为问题"

    # window_title 在任何位置都不允许。
    assert privacy.validate_payload({
        "type": TYPE_AGENT_TASK_SUMMARY,
        "context": {"app": "ZCode"},
        "data": {"window_title": "x"},
    }) != []


def test_event_round_trips_through_sqlite(tmp_path) -> None:
    """任务摘要走既有的本地队列，离线也能落盘。"""
    from lumen_desktop.storage import Storage

    storage = Storage(str(tmp_path / "lumen.db"))
    try:
        event = make_task_event()
        assert storage.store_event(event) is True

        # 幂等：同一个 event_id 再写一次不新增。
        assert storage.store_event(event) is False

        pending = storage.pending_events()
        assert len(pending) == 1
        assert pending[0].type == TYPE_AGENT_TASK_SUMMARY
        assert pending[0].data["task_id"] == "zcode-2026-09-17-001"
        # 序列化后的内容里不应出现任何正文类字段名。
        raw = json.dumps(pending[0].to_payload(), ensure_ascii=False)
        for forbidden in ("conversation", "terminal_output", "diff", "source_code"):
            assert forbidden not in raw
    finally:
        storage.close()
