"""Git 传感器测试。

重点验证两条边界：
1. 首次扫描只建立基线，不产生事件（否则每次重启都会刷一批噪声事件）；
2. 真实变化（新提交、切分支）才产生事件，且事件里不含路径、remote、diff。
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from lumen_desktop.config import Config
from lumen_desktop.privacy import PrivacyFilter
from lumen_desktop.sensors.git import GitSensor


def _git(repo: Path, *args: str) -> None:
    """在指定仓库执行 git 命令。"""
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "Test",
        "GIT_AUTHOR_EMAIL": "test@example.com",
        "GIT_COMMITTER_NAME": "Test",
        "GIT_COMMITTER_EMAIL": "test@example.com",
    }
    subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True, env=env)


def _make_repo(path: Path, name: str = "testrepo") -> Path:
    """创建一个带初始提交的临时仓库。"""
    repo = path / name
    repo.mkdir(parents=True)
    _git(repo, "init", "-q", "-b", "main")
    (repo / "a.txt").write_text("hello\n")
    _git(repo, "add", "-A")
    _git(repo, "commit", "-q", "-m", "feat: 初始提交")
    return repo


@pytest.fixture
def sensor_env(tmp_path):
    """构造一个指向临时仓库的传感器。"""
    repo = _make_repo(tmp_path)
    events: list = []
    config = Config(repo_roots=[str(repo)], project_keywords={repo.name: [repo.name]})
    sensor = GitSensor(config, PrivacyFilter(config.project_keywords), events.append)
    return sensor, repo, events


class TestBaseline:
    def test_first_scan_emits_no_event(self, sensor_env):
        """回归测试：首次扫描不应产生事件。

        这里曾经有个 bug —— 首次见到仓库会发一条 workspace 事件，
        而采集端每次重启都会重新扫描，于是每个仓库每次重启都刷一条事件。
        设计文档明确要求"仅状态变化时生成事件"。
        """
        sensor, repo, events = sensor_env
        sensor._repos = sensor._discover_repos()

        for r in sensor._repos:
            sensor._check_repo(r)

        assert events == [], f"首次扫描不应产生事件，实际产生了 {len(events)} 条"

    def test_repeated_scan_without_change_emits_nothing(self, sensor_env):
        """无变化时反复扫描不应产生事件。"""
        sensor, repo, events = sensor_env
        sensor._repos = sensor._discover_repos()

        for _ in range(3):
            for r in sensor._repos:
                sensor._check_repo(r)

        assert events == []


class TestChangeDetection:
    def test_new_commit_emits_event(self, sensor_env):
        sensor, repo, events = sensor_env
        sensor._repos = [repo]
        sensor._check_repo(repo)  # 建立基线
        assert events == []

        (repo / "b.txt").write_text("world\n")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-q", "-m", "feat: 新增功能")
        sensor._check_repo(repo)

        assert len(events) == 1
        payload = events[0].to_payload()
        assert payload["type"] == "git.activity"
        assert payload["data"]["kind"] == "commit"
        assert payload["data"]["commit_message"] == "feat: 新增功能"
        assert payload["privacy"] == "P1"

    def test_branch_switch_emits_workspace_event(self, sensor_env):
        sensor, repo, events = sensor_env
        sensor._repos = [repo]
        sensor._check_repo(repo)

        _git(repo, "checkout", "-q", "-b", "feature/x")
        sensor._check_repo(repo)

        assert len(events) == 1
        assert events[0].to_payload()["data"]["kind"] == "workspace"
        assert events[0].to_payload()["data"]["branch"] == "feature/x"

    def test_changed_files_count_is_recorded(self, sensor_env):
        sensor, repo, events = sensor_env
        sensor._repos = [repo]
        sensor._check_repo(repo)

        for name in ("x.txt", "y.txt", "z.txt"):
            (repo / name).write_text("content\n")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-q", "-m", "feat: 三个文件")
        sensor._check_repo(repo)

        assert events[0].to_payload()["data"]["changed_files_count"] == 3


class TestPrivacyBoundary:
    def test_event_never_contains_path_remote_or_diff(self, sensor_env):
        """事件中绝不能出现绝对路径、remote URL 或 diff 内容。"""
        sensor, repo, events = sensor_env
        # 故意配置一个 remote，确认它不会进入事件
        _git(repo, "remote", "add", "origin", "https://github.com/example/secret-repo.git")

        sensor._repos = [repo]
        sensor._check_repo(repo)
        (repo / "c.txt").write_text("secret content here\n")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-q", "-m", "fix: 修改文件")
        sensor._check_repo(repo)

        import json

        raw = json.dumps([e.to_payload() for e in events], ensure_ascii=False)
        for forbidden in ("/tmp/", "/Users/", "github.com", "secret-repo", "secret content", "diff", "origin"):
            assert forbidden not in raw, f"事件中不应出现 {forbidden}"

    def test_commit_message_with_secret_is_sanitized(self, sensor_env):
        """提交信息里夹带凭证时必须被脱敏。"""
        sensor, repo, events = sensor_env
        sensor._repos = [repo]
        sensor._check_repo(repo)

        # 模拟一条夹带密钥与绝对路径的提交信息
        fake_key = "sk-" + "deadbeef12345678"  # secrets-check:allow
        _git(repo, "commit", "-q", "--allow-empty",
             "-m", f"fix: 用 {fake_key} 调用 /Users/liang/secret/config.json")

        sensor._check_repo(repo)

        message = events[0].to_payload()["data"]["commit_message"]
        assert fake_key not in message, "密钥未被脱敏"
        assert "/Users/liang" not in message, "路径未被脱敏"
        assert "secret" not in message or "[路径]" in message
