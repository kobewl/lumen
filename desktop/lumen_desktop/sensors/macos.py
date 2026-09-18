"""macOS 平台探针：锁屏判定、空闲时长、系统伪应用识别。

三个探针集中在这里的原因：
- 锁屏判定只能有一个实现，否则 Idle 与 Window 两个传感器可能给出不一致的结论；
- 系统伪应用清单必须与服务端保持一致（服务端另有一份，用于重算历史数据）。

依赖 pyobjc/Quartz，缺失时全部降级为「未知」，绝不误报锁屏。
"""

from __future__ import annotations

import logging

logger = logging.getLogger(__name__)

# 系统伪应用：锁屏、屏保与系统认证窗口。
# 它们出现在前台**不代表**用户在工作，绝不能被计为工作活动。
#
# 清单必须与 server/internal/events/systemapps.go 保持一致：
# 采集端过滤是为了不再产生脏数据，服务端过滤是为了历史脏数据在重算时被忽略。
_SYSTEM_APP_NAMES = {
    "loginwindow",
    "screensaverengine",
    "securityagent",
    "coreautha",
    "coreauthui",
    "loginstatus",
}

# bundle id 前缀匹配，比名称更稳（本地化名称会随系统语言变化）。
_SYSTEM_BUNDLE_PREFIXES = (
    "com.apple.loginwindow",
    "com.apple.screensaver",
    "com.apple.securityagent",
    "com.apple.coreauthui",
)

try:  # pragma: no cover - 依赖运行环境
    from Quartz import (  # type: ignore
        CGEventSourceSecondsSinceLastEventType,
        CGSessionCopyCurrentDictionary,
        kCGAnyInputEventType,
        kCGEventSourceStateCombinedSessionState,
    )

    QUARTZ_AVAILABLE = True
except Exception:  # pragma: no cover
    QUARTZ_AVAILABLE = False


def is_system_pseudo_app(app: str, bundle_id: str = "") -> bool:
    """判断前台应用是否属于系统伪应用（锁屏、屏保、认证窗口）。"""
    name = (app or "").strip().lower()
    if name and name in _SYSTEM_APP_NAMES:
        return True
    bundle = (bundle_id or "").strip().lower()
    if bundle:
        for prefix in _SYSTEM_BUNDLE_PREFIXES:
            if bundle.startswith(prefix):
                return True
    return False


def screen_is_locked() -> bool:
    """会话是否处于锁屏状态。取不到信息时返回 False（不误报）。"""
    if not QUARTZ_AVAILABLE:
        return False
    try:
        session = CGSessionCopyCurrentDictionary()
        if not session:
            return False
        # 键名在不同系统版本上略有差异，两个都检查。
        return bool(session.get("CGSSessionScreenIsLocked")) or session.get("kCGSessionOnConsoleKey") == 0
    except Exception:
        return False


def seconds_since_last_input() -> float:
    """返回自上次输入事件以来的秒数；取不到时返回 0。"""
    if not QUARTZ_AVAILABLE:
        return 0.0
    try:
        return float(
            CGEventSourceSecondsSinceLastEventType(
                kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType
            )
        )
    except Exception:
        return 0.0
