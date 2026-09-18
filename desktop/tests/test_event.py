"""事件模型与隐私 allowlist 测试。

这些测试是隐私边界的第一道防线：任何试图把敏感内容塞进事件的代码
都应该在这里失败。
"""

from __future__ import annotations

from datetime import datetime, timezone

import pytest

from lumen_desktop.event import (
    ALLOWED_CONTEXT_KEYS,
    ALLOWED_DATA_KEYS,
    Event,
    PrivacyError,
)
from lumen_desktop.privacy import PrivacyFilter


def _utc(year=2026, month=9, day=17, hour=9):
    return datetime(year, month, day, hour, 0, 0, tzinfo=timezone.utc)


class TestEventAllowlist:
    def test_window_event_only_contains_allowed_fields(self):
        event = Event.window_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            start=_utc(),
            duration_seconds=320.7,
            app="Visual Studio Code",
            bundle_id="com.microsoft.VSCode",
            project="lumen",
        )
        payload = event.to_payload()
        assert set(payload["context"]) <= ALLOWED_CONTEXT_KEYS
        assert set(payload["data"]) <= ALLOWED_DATA_KEYS
        assert payload["data"]["duration_seconds"] == 320
        assert payload["privacy"] == "P0"

    def test_rejects_forbidden_field_in_context(self):
        with pytest.raises(PrivacyError, match="window_title"):
            Event(
                id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
                device_id="desktop-mac-01",
                type="window.activity",
                timestamp=_utc(),
                privacy="P0",
                context={"app": "VS Code", "window_title": "Secret Project"},
                data={"duration_seconds": 10},
            )

    def test_rejects_forbidden_field_in_data(self):
        with pytest.raises(PrivacyError, match="clipboard"):
            Event(
                id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
                device_id="desktop-mac-01",
                type="window.activity",
                timestamp=_utc(),
                privacy="P0",
                context={"app": "VS Code"},
                data={"duration_seconds": 10, "clipboard": "secret"},
            )

    def test_rejects_undeclared_field(self):
        with pytest.raises(PrivacyError, match="未声明字段"):
            Event(
                id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
                device_id="desktop-mac-01",
                type="window.activity",
                timestamp=_utc(),
                privacy="P0",
                context={"app": "VS Code", "site": "example.com"},
                data={"duration_seconds": 10},
            )

    def test_rejects_unknown_type(self):
        with pytest.raises(PrivacyError, match="未知事件类型"):
            Event(
                id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
                device_id="desktop-mac-01",
                type="clipboard.copy",
                timestamp=_utc(),
                privacy="P0",
            )

    def test_timestamp_is_utc_rfc3339(self):
        event = Event.window_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            start=_utc(),
            duration_seconds=60,
            app="Safari",
        )
        assert event.to_payload()["timestamp"] == "2026-09-17T09:00:00Z"


class TestEventFactories:
    def test_idle_state_validates_state(self):
        with pytest.raises(PrivacyError, match="非法 idle 状态"):
            Event.idle_state(
                event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
                device_id="desktop-mac-01",
                start=_utc(),
                state="sleeping",
            )

    def test_idle_state_accepts_locked(self):
        event = Event.idle_state(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            start=_utc(),
            state="locked",
            duration_seconds=120,
        )
        assert event.to_payload()["data"]["state"] == "locked"

    def test_git_event_truncates_commit_message(self):
        long_message = "feat: " + "x" * 500
        event = Event.git_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            when=_utc(),
            repo="lumen",
            commit_message=long_message,
        )
        assert len(event.to_payload()["data"]["commit_message"]) <= 200

    def test_git_event_keeps_only_first_line(self):
        event = Event.git_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            when=_utc(),
            repo="lumen",
            commit_message="feat: session engine\n\nbody with details",
        )
        assert event.to_payload()["data"]["commit_message"] == "feat: session engine"

    def test_git_event_never_contains_path_or_remote(self):
        """Git 事件不允许出现路径或 remote URL 字段。"""
        event = Event.git_activity(
            event_id="01J9Z4QK7M3F8N2P5R7T9V1X3B",
            device_id="desktop-mac-01",
            when=_utc(),
            repo="lumen",
            kind="commit",
            head_commit="a1b2c3d",
        )
        payload = event.to_payload()
        for forbidden in ("path", "absolute_path", "remote_url", "diff", "source_code"):
            assert forbidden not in payload["context"]
            assert forbidden not in payload["data"]


class TestPrivacyFilter:
    def test_sanitize_removes_paths_and_urls(self):
        f = PrivacyFilter()
        dirty = "fix /Users/liang/code/lumen and https://github.com/liang/lumen.git"
        clean = f.sanitize_text(dirty)
        assert "/Users/liang" not in clean
        assert "github.com" not in clean

    def test_sanitize_removes_entire_path_not_just_username(self):
        """回归测试：只替换用户名会残留后续路径片段，必须整条路径替换。"""
        f = PrivacyFilter()
        clean = f.sanitize_text("/Users/liang/secret/project/config.json")
        assert "/Users/liang" not in clean
        assert "secret/project" not in clean
        assert "config.json" not in clean

    def test_sanitize_handles_multiple_path_styles(self):
        f = PrivacyFilter()
        for raw in (
            "/home/liang/work/secret.py",
            "/var/log/lumen/token.log",
            "/private/var/db/secret",
            r"C:\Users\liang\Documents\secret.txt",
        ):
            clean = f.sanitize_text(raw)
            assert "liang" not in clean, raw
            assert "secret" not in clean, raw

    def test_sanitize_removes_secret_like_tokens(self):
        # 这里刻意使用"看起来像密钥"的假值，用来验证脱敏规则本身生效。
        f = PrivacyFilter()
        fake_key = "sk-" + "abcdef1234567890"  # secrets-check:allow
        clean = f.sanitize_text(f"config {fake_key} done")
        assert fake_key not in clean

    def test_sanitize_removes_password_assignment(self):
        f = PrivacyFilter()
        clean = f.sanitize_text("password=hunter2")
        assert "hunter2" not in clean

    def test_contains_sensitive_detects_paths(self):
        f = PrivacyFilter()
        assert f.contains_sensitive("/home/liang/work")
        assert f.contains_sensitive("https://example.com/x")
        assert not f.contains_sensitive("just a normal message")

    def test_project_from_title_requires_whitelist(self):
        f = PrivacyFilter({"lumen": ["lumen"], "clipmaster": ["clipmaster"]})
        assert f.project_from_title("lumen_desktop/sync.py - Visual Studio Code") == "lumen"
        assert f.project_from_title("ClipMaster Pro") == "clipmaster"
        # 未命中白名单时必须返回 None，标题本身不会上传。
        assert f.project_from_title("个人简历 - Pages") is None
        assert f.project_from_title("") is None

    def test_validate_payload_flags_sensitive_values(self):
        f = PrivacyFilter({"lumen": ["lumen"]})
        problems = f.validate_payload({
            "context": {"app": "Terminal", "project": "/Users/liang/secret"},
            "data": {"duration_seconds": 10},
        })
        assert problems, "含绝对路径的 payload 应被标记"

    def test_validate_payload_checks_every_string_field(self):
        """回归测试：不能对某些字段名做豁免，取值必须逐字段检查。"""
        f = PrivacyFilter({"lumen": ["lumen"]})
        for field, value in (
            ("app", "/Users/liang/Applications/Secret.app"),
            ("project", "/home/liang/proj"),
            ("repo", "https://git.example.com/liang/repo.git"),
            ("branch", "/private/tmp/branch"),
        ):
            problems = f.validate_payload({
                "context": {field: value},
                "data": {"duration_seconds": 10},
            })
            assert problems, f"{field}={value} 应被标记"

    def test_validate_payload_flags_window_title_field(self):
        f = PrivacyFilter()
        problems = f.validate_payload({
            "context": {"app": "Chrome"},
            "data": {"window_title": "银行页面"},
        })
        assert any("窗口标题" in p for p in problems)

    def test_validate_payload_passes_clean_event(self):
        f = PrivacyFilter({"lumen": ["lumen"]})
        problems = f.validate_payload({
            "context": {"app": "Visual Studio Code", "project": "lumen"},
            "data": {"duration_seconds": 320, "checkpoint": True},
        })
        assert problems == []
