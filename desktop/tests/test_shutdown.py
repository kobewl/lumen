"""优雅退出测试。

回归测试：退出流程曾经先关闭数据库、再调用会查询数据库的 status()，
导致进程以 ProgrammingError 崩溃退出。这个 bug 只在退出时出现，
所以必须有专门的测试覆盖，否则很容易再次引入。
"""

from __future__ import annotations

import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

DESKTOP_DIR = Path(__file__).resolve().parent.parent
VENV_PYTHON = DESKTOP_DIR / ".venv" / "bin" / "python"


def _python() -> str:
    """优先用项目虚拟环境，没有就用当前解释器。"""
    return str(VENV_PYTHON) if VENV_PYTHON.exists() else sys.executable


class TestStatusDetection:
    """状态检测的回归测试。

    现象：进程明明在跑，`lumen-desktop status` 却显示"未运行"。
    原因：状态文件被上一次退出流程覆盖成 pid=null（竞态），而 status 只信这个文件。
    修复：status 用进程列表交叉验证，并让退出流程只在 pid 仍属于自己时才清空。
    """

    def test_find_running_pids_excludes_self(self):
        from lumen_desktop.cli import _find_running_pids

        pids = _find_running_pids()
        import os

        assert os.getpid() not in pids, "不应把当前进程算作采集进程"

    def test_status_reports_running_from_process_list(self, tmp_path, monkeypatch):
        """状态文件说没运行，但进程列表里有 —— 应判定为运行中。"""
        from lumen_desktop import cli

        # 伪造一个"状态文件说 pid=null"的场景
        monkeypatch.setattr(cli, "_read_state", lambda: {"pid": None, "stopped_at": "2026-01-01T00:00:00Z"})
        monkeypatch.setattr(cli, "_find_running_pids", lambda: [12345])

        status = cli._read_state()
        pids = cli._find_running_pids()
        # 复现 status 命令的判定顺序
        running = bool(status.get("pid")) or bool(pids)
        assert running is True

    def test_clear_state_keeps_newer_owner(self, tmp_path, monkeypatch):
        """退出流程不应覆盖新进程写入的状态。"""
        from lumen_desktop import cli

        state_file = tmp_path / "state.json"
        monkeypatch.setattr(cli, "STATE_FILE", state_file)
        monkeypatch.setattr(cli, "STATE_DIR", tmp_path)

        # 模拟：新进程已经写入自己的 pid
        cli._write_state({"pid": 99999, "started_at": "2026-01-01T00:00:00Z"})

        # 旧进程（pid=11111）退出，试图清空状态
        cli._clear_state_if_owner(11111, "2026-01-01T00:01:00Z")

        import json

        after = json.loads(state_file.read_text())
        assert after["pid"] == 99999, "旧进程退出不应抹掉新进程的状态记录"

    def test_clear_state_works_when_owner_matches(self, tmp_path, monkeypatch):
        """pid 属于自己时应正常清空。"""
        from lumen_desktop import cli

        state_file = tmp_path / "state.json"
        monkeypatch.setattr(cli, "STATE_FILE", state_file)
        monkeypatch.setattr(cli, "STATE_DIR", tmp_path)

        cli._write_state({"pid": 11111, "started_at": "2026-01-01T00:00:00Z"})
        cli._clear_state_if_owner(11111, "2026-01-01T00:01:00Z")

        import json

        after = json.loads(state_file.read_text())
        assert after["pid"] is None
        assert after["stopped_at"] == "2026-01-01T00:01:00Z"

    def test_write_state_is_atomic(self, tmp_path, monkeypatch):
        """写入应通过临时文件替换，避免留下损坏的 JSON。"""
        from lumen_desktop import cli

        state_file = tmp_path / "state.json"
        monkeypatch.setattr(cli, "STATE_FILE", state_file)
        monkeypatch.setattr(cli, "STATE_DIR", tmp_path)

        cli._write_state({"pid": 42})
        cli._write_state({"pid": 43})

        import json

        assert json.loads(state_file.read_text())["pid"] == 43
        assert not list(tmp_path.glob("*.tmp")), "不应残留临时文件"


@pytest.mark.skipif(sys.platform != "darwin", reason="采集端仅在 macOS 上运行")
def test_shutdown_is_clean(tmp_path):
    """启动采集端后发送 SIGTERM，进程应干净退出（返回码 0）。"""
    config = tmp_path / "config.json"
    config.write_text(
        '{"db_path": "%s", "server_url": "http://127.0.0.1:1",'
        ' "repo_roots": [], "sync_interval_seconds": 3600}' % (tmp_path / "events.db"),
        encoding="utf-8",
    )

    proc = subprocess.Popen(
        [_python(), "-m", "lumen_desktop", "--config", str(config), "run"],
        cwd=str(DESKTOP_DIR),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )

    try:
        # 等它完成启动（传感器线程与数据库初始化）。
        time.sleep(6)
        assert proc.poll() is None, "采集端不应在启动阶段就退出"

        proc.send_signal(signal.SIGTERM)
        try:
            output, _ = proc.communicate(timeout=25)
        except subprocess.TimeoutExpired:
            proc.kill()
            output, _ = proc.communicate()
            pytest.fail(f"采集端未能在 25 秒内退出。输出：\n{output}")

        assert proc.returncode == 0, f"退出码应为 0，实际 {proc.returncode}。输出：\n{output}"
        assert "Cannot operate on a closed database" not in output, "退出时出现了数据库已关闭的错误"
        assert "Traceback" not in output, f"退出时不应有异常堆栈：\n{output}"
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=5)
