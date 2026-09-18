"""task-summary CLI 入口测试。

这个入口是专业 Agent（先支持 ZCode）提交任务摘要的唯一方式。
它必须满足三点：写本地队列而不是直接发网络请求（离线也能用）、
非法输入在本地就被拒绝、重复提交是幂等的。
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from lumen_desktop import cli  # noqa: E402
from lumen_desktop.storage import Storage  # noqa: E402


@pytest.fixture()
def config_file(tmp_path, monkeypatch) -> Path:
    """写一份指向临时目录的配置，避免碰到真实的 ~/.lumen。"""
    db_path = tmp_path / "lumen.db"
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({
        "device_id": "desktop-mac-01",
        "server_url": "http://127.0.0.1:9",
        "db_path": str(db_path),
        "repo_roots": [],
    }), encoding="utf-8")
    return cfg


def run_cli(config_file: Path, *args: str) -> int:
    return cli.main(["--config", str(config_file), *args])


def read_events(config_file: Path) -> list[dict]:
    config = json.loads(config_file.read_text(encoding="utf-8"))
    storage = Storage(config["db_path"])
    try:
        return storage.recent_events(limit=50)
    finally:
        storage.close()


def test_cli_queues_task_summary(config_file, capsys) -> None:
    """正常提交：写入本地队列，状态是 ready_to_sync。"""
    code = run_cli(
        config_file,
        "task-summary",
        "--task-id", "zcode-001",
        "--title", "接通任务摘要链路",
        "--status", "done",
        "--outcome", "新增事件类型",
        "--open-loop", "尚未接入真实上报",
        "--project", "lumen",
        "--json",
    )
    assert code == 0

    out = json.loads(capsys.readouterr().out)
    assert out["status"] == "queued"
    assert out["task_id"] == "zcode-001"

    events = read_events(config_file)
    assert len(events) == 1
    event = events[0]
    assert event["type"] == "agent.task_summary"
    assert event["privacy"] == "P1"
    assert event["data"]["title"] == "接通任务摘要链路"
    assert event["data"]["outcomes"] == ["新增事件类型"]
    assert event["data"]["open_loops"] == ["尚未接入真实上报"]
    assert event["data"]["privacy_mode"] == "metadata_only"


def test_cli_supports_multiline_arguments(config_file) -> None:
    """Agent 一次产出多条，支持多行文本传入更省事。"""
    code = run_cli(
        config_file,
        "task-summary",
        "--task-id", "zcode-002",
        "--title", "多行条目",
        "--outcome", "第一条\n第二条\n第三条",
        "--json",
    )
    assert code == 0

    events = read_events(config_file)
    assert events[0]["data"]["outcomes"] == ["第一条", "第二条", "第三条"]


def test_cli_rejects_path_in_title(config_file, capsys) -> None:
    """标题里夹带绝对路径时本地就拒绝，并记录原因（不记录正文）。"""
    code = run_cli(
        config_file,
        "task-summary",
        "--task-id", "zcode-003",
        "--title", "改了 /Users/liang/Documents/Project/lumen/x.go",
    )
    assert code == 2
    err = capsys.readouterr().err
    assert "拒绝" in err or "隐私校验" in err

    # 事件不应入队。
    assert read_events(config_file) == []

    # 失败原因要留痕，但不能把被拒绝的正文写进去。
    config = json.loads(config_file.read_text(encoding="utf-8"))
    storage = Storage(config["db_path"])
    try:
        recorded = storage.get_meta("last_task_summary_error") or ""
    finally:
        storage.close()
    assert "zcode-003" in recorded
    assert "/Users/liang" not in recorded


def test_cli_rejects_oversized_title(config_file, capsys) -> None:
    """超长标题在本地被拒绝，而不是截断后照常提交。"""
    code = run_cli(
        config_file,
        "task-summary",
        "--task-id", "zcode-004",
        "--title", "字" * 200,
    )
    assert code == 2
    assert "拒绝" in capsys.readouterr().err
    assert read_events(config_file) == []


def test_cli_rejects_bad_status(config_file) -> None:
    """状态取值由 argparse 限制，非法值直接报错退出。"""
    with pytest.raises(SystemExit):
        run_cli(config_file, "task-summary",
                "--task-id", "zcode-005", "--title", "标题", "--status", "in_progress")


def test_cli_rejects_bad_occurred_at(config_file, capsys) -> None:
    """时间格式非法时给出明确错误，而不是静默用当前时间。"""
    code = run_cli(
        config_file,
        "task-summary",
        "--task-id", "zcode-006",
        "--title", "标题",
        "--occurred-at", "昨天下午",
    )
    assert code == 2
    assert "RFC3339" in capsys.readouterr().err
    assert read_events(config_file) == []


def test_cli_is_idempotent_for_same_event(config_file) -> None:
    """同一 task_id 提交两次会生成两条事件，但 event_id 不同、都合法。

    幂等的粒度在服务端是 (device_id, task_id)：Agent 常先报"进行中"、
    结束时再报"已完成"，两次都应该被接受，由服务端保留最新状态。
    这里确认本地入口不会阻止 Agent 的二次汇报。
    """
    for status in ("partial", "done"):
        code = run_cli(
            config_file,
            "task-summary",
            "--task-id", "zcode-007",
            "--title", "先报进行中再报完成",
            "--status", status,
            "--json",
        )
        assert code == 0

    events = read_events(config_file)
    assert len(events) == 2
    assert {e["data"]["status"] for e in events} == {"partial", "done"}


def test_cli_defaults_are_conservative(config_file) -> None:
    """默认值刻意保守：状态 unknown、无项目、无结果条目。"""
    code = run_cli(config_file, "task-summary",
                   "--task-id", "zcode-008", "--title", "只有标题", "--json")
    assert code == 0

    data = read_events(config_file)[0]["data"]
    assert data["status"] == "unknown", "Agent 没给结论时不要替它猜"
    assert data["outcomes"] == []
    assert data["open_loops"] == []
    assert "project" not in read_events(config_file)[0]["context"]


def test_cli_never_accepts_content_flags(config_file) -> None:
    """入口本身不提供任何正文类参数。

    完整对话、终端输出、代码与 diff 没有对应的命令行选项，
    因此 Agent 无法通过这个入口把它们传进来。
    """
    parser = cli.build_parser()
    subparsers = [a for a in parser._actions if hasattr(a, "choices") and a.choices]
    task_parser = None
    for action in subparsers:
        if "task-summary" in action.choices:
            task_parser = action.choices["task-summary"]
    assert task_parser is not None

    option_strings = set()
    for action in task_parser._actions:
        option_strings.update(action.option_strings)

    for forbidden in ("--conversation", "--terminal-output", "--code", "--diff",
                      "--patch", "--transcript", "--messages"):
        assert forbidden not in option_strings, f"不应存在 {forbidden} 参数"
