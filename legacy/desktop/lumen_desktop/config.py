"""桌面端配置。

安全约定：配置文件里**不允许**出现任何服务端密钥、DeepSeek Key 或飞书凭证。
device_token 存在 macOS Keychain（见 keychain.py），不写入配置文件。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field, fields
from pathlib import Path
from typing import Any

# 默认路径都放在用户目录下，避免把数据写进仓库目录。
DEFAULT_DIR = Path.home() / ".lumen"
DEFAULT_CONFIG_PATH = DEFAULT_DIR / "config.json"
DEFAULT_DB_PATH = DEFAULT_DIR / "data" / "events.db"


@dataclass
class Config:
    """运行配置。所有时间单位为秒。"""

    # 服务端
    server_url: str = "http://127.0.0.1:8787"
    device_id: str = "desktop-mac-01"

    # 本地存储
    db_path: str = str(DEFAULT_DB_PATH)

    # 传感器开关
    enable_window_sensor: bool = True
    enable_idle_sensor: bool = True
    enable_git_sensor: bool = True

    # 暂停采集（也可用 CLI pause/resume 切换）
    paused: bool = False

    # 采样与检查点
    window_poll_seconds: float = 2.0
    window_checkpoint_seconds: float = 60.0
    idle_poll_seconds: float = 5.0
    git_poll_seconds: float = 60.0
    sync_interval_seconds: float = 300.0

    # idle 判定
    #
    # 5 分钟无输入进入 idle：采集端在此时停止窗口计时（宁可少算，不多算）。
    # 会话边界（idle 超过 8 分钟切断）由服务端 Session 引擎负责，
    # 见 server/internal/sessions/engine.go 的 GapThreshold。
    idle_threshold_seconds: float = 300.0

    # Git：只扫描显式配置的目录，绝不递归扫描整个磁盘
    repo_roots: list[str] = field(default_factory=list)

    # 应用黑名单：命中后只记录 idle，不记录 window 事件
    app_blacklist: list[str] = field(default_factory=list)

    # 项目白名单：窗口标题只有在命中这些关键词后才允许裁剪成 project_hint
    project_keywords: dict[str, list[str]] = field(default_factory=dict)

    # 同步阈值
    sync_batch_max_events: int = 100
    sync_batch_max_bytes: int = 512 * 1024

    # 队列与保留
    synced_retention_days: int = 7
    queue_soft_limit_mb: int = 100
    queue_hard_limit_mb: int = 500

    # 日志
    log_level: str = "INFO"

    @classmethod
    def load(cls, path: Path | str | None = None) -> "Config":
        """从 JSON 文件读取配置；文件不存在时返回默认值。"""
        config_path = Path(path) if path else DEFAULT_CONFIG_PATH
        if not config_path.exists():
            return cls()

        with config_path.open("r", encoding="utf-8") as fh:
            raw = json.load(fh)
        return cls.from_dict(raw)

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "Config":
        """按字段白名单构造配置。

        未知字段直接忽略，避免配置文件里意外塞入凭证后被程序使用。
        """
        known = {f.name for f in fields(cls)}
        filtered = {k: v for k, v in raw.items() if k in known}
        cfg = cls(**filtered)
        cfg.repo_roots = [str(Path(p).expanduser()) for p in cfg.repo_roots]
        return cfg

    def save(self, path: Path | str | None = None) -> Path:
        """保存配置到 JSON 文件，权限 0600（仅本人可读）。"""
        config_path = Path(path) if path else DEFAULT_CONFIG_PATH
        config_path.parent.mkdir(parents=True, exist_ok=True)
        data = {f.name: getattr(self, f.name) for f in fields(self)}
        with config_path.open("w", encoding="utf-8") as fh:
            json.dump(data, fh, ensure_ascii=False, indent=2)
        os.chmod(config_path, 0o600)
        return config_path

    def ensure_dirs(self) -> None:
        """确保数据目录存在，并设置较严的权限。"""
        db_dir = Path(self.db_path).parent
        db_dir.mkdir(parents=True, exist_ok=True)
        os.chmod(db_dir, 0o700)

    def project_for_title(self, title: str) -> str | None:
        """按项目白名单从窗口标题中提取项目名。

        只有命中白名单关键词才返回项目名；否则返回 None（标题本身永不上传）。
        """
        if not title:
            return None
        lowered = title.lower()
        for project, keywords in self.project_keywords.items():
            for kw in keywords:
                if kw.lower() in lowered:
                    return project
        return None

    def is_blacklisted(self, app: str, bundle_id: str = "") -> bool:
        """判断应用是否在黑名单中。"""
        for item in self.app_blacklist:
            if not item:
                continue
            item_lower = item.lower()
            if item_lower == app.lower() or (bundle_id and item_lower == bundle_id.lower()):
                return True
        return False


def default_config_template() -> dict[str, Any]:
    """生成一份不含任何密钥的配置样例。"""
    return {
        "server_url": "http://127.0.0.1:8787",
        "device_id": "desktop-mac-01",
        "db_path": str(DEFAULT_DB_PATH),
        "enable_window_sensor": True,
        "enable_idle_sensor": True,
        "enable_git_sensor": True,
        "paused": False,
        "window_poll_seconds": 2.0,
        "window_checkpoint_seconds": 60.0,
        "idle_poll_seconds": 5.0,
        "git_poll_seconds": 60.0,
        "sync_interval_seconds": 300.0,
        "idle_threshold_seconds": 300.0,
        "repo_roots": ["~/Documents/Project"],
        "app_blacklist": ["1Password", "Keychain Access", "com.apple.keychainaccess"],
        "project_keywords": {"lumen": ["lumen"], "clipmaster": ["clipmaster"]},
    }
