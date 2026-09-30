"""本地隐私过滤。

V0.1 不采集剪贴板和截图，因此隐私引擎收敛为四件事：
1. 字段 allowlist（由 event.Event 强制）；
2. 移除绝对路径、用户名、URL；
3. 窗口标题默认丢弃，只有命中项目白名单才转成 project 名；
4. 黑名单应用只保留 idle，不产生 window 事件。

日志也只记录事件 id/type/status，不记录完整 data。
"""

from __future__ import annotations

import re
from typing import Any

# 绝对路径的检测规则。
#
# 注意这里用 [^\s]* 而不是 [^/\s]+：如果只匹配到用户名就停下，
# 形如 "<用户目录>/secret/config.json" 的路径会被截成 "[路径]/secret/config.json"，
# 后续路径片段依然泄露。必须把整条路径（到空白为止）都替换掉。
USER_PATH_PATTERNS = [
    re.compile(r"/Users/[^\s]*"),
    re.compile(r"/home/[^\s]*"),
    re.compile(r"/var/[^\s]*"),
    re.compile(r"/private/[^\s]*"),
    re.compile(r"[A-Z]:\\Users\\[^\s]*", re.IGNORECASE),
]

# URL 检测，避免 remote URL 或网页地址混入。
URL_PATTERN = re.compile(r"\b(?:https?|ssh|git|ftp)://\S+", re.IGNORECASE)

# 需要屏蔽的敏感文本模式（用于 commit message 等自由文本字段）。
SECRET_PATTERNS = [
    re.compile(r"\b(sk|pk|ak)-[A-Za-z0-9]{8,}\b"),          # API key 形态
    re.compile(r"\b[A-Za-z0-9_]*token[A-Za-z0-9_]*\s*[:=]\s*\S+", re.IGNORECASE),
    re.compile(r"\bpassword\s*[:=]\s*\S+", re.IGNORECASE),
    re.compile(r"\bBearer\s+[A-Za-z0-9._\-]+", re.IGNORECASE),
]

MAX_TEXT_LENGTH = 200


class PrivacyFilter:
    """执行文本裁剪与敏感内容检测。"""

    def __init__(self, project_keywords: dict[str, list[str]] | None = None):
        self.project_keywords = project_keywords or {}

    def sanitize_text(self, text: str, limit: int = MAX_TEXT_LENGTH) -> str:
        """移除路径、URL 与疑似凭证，并截断长度。

        用于 commit message 这类必须上传但可能夹带敏感信息的文本。
        """
        if not text:
            return ""

        result = text
        for pattern in USER_PATH_PATTERNS:
            result = pattern.sub("[路径]", result)
        result = URL_PATTERN.sub("[链接]", result)
        for pattern in SECRET_PATTERNS:
            result = pattern.sub("[已过滤]", result)

        result = result.replace("\n", " ").replace("\r", " ").strip()
        if len(result) > limit:
            result = result[:limit]
        return result

    def contains_sensitive(self, text: str) -> bool:
        """判断文本是否仍含敏感内容，用于上传前的最后一道检查。"""
        if not text:
            return False
        return any(p.search(text) for p in (*USER_PATH_PATTERNS, URL_PATTERN, *SECRET_PATTERNS))

    def project_from_title(self, title: str) -> str | None:
        """从窗口标题中提取项目名。

        只有命中配置的关键词才返回项目名；未命中一律返回 None。
        标题本身不会被上传，也不会被存储到事件里。
        """
        if not title:
            return None
        lowered = title.lower()
        for project, keywords in self.project_keywords.items():
            for keyword in keywords:
                if keyword and keyword.lower() in lowered:
                    return project
        return None

    def validate_payload(self, payload: dict[str, Any]) -> list[str]:
        """上传前校验 payload，返回发现的问题列表（空列表表示通过）。

        这是上传前的最后一道防线：即使 Event 的 allowlist 已经限制了字段名，
        字段的**取值**仍可能夹带敏感内容（例如项目名被误写成路径），
        因此这里对每个字符串值都做一次内容检查，不做任何字段豁免。
        """
        problems: list[str] = []

        context = payload.get("context") or {}
        data = payload.get("data") or {}
        is_task_summary = payload.get("type") == "agent.task_summary"

        for scope, fields in (("context", context), ("data", data)):
            for key, value in fields.items():
                if isinstance(value, str) and self.contains_sensitive(value):
                    problems.append(f"{scope}.{key} 含路径、URL 或凭证特征")
                # 任务摘要的条目数组比单个字符串更容易夹带长文本，逐条检查。
                if isinstance(value, list):
                    for item in value:
                        if isinstance(item, str) and self.contains_sensitive(item):
                            problems.append(f"{scope}.{key} 含路径、URL 或凭证特征")

        # 窗口标题绝不允许出现。
        #
        # data.title 是唯一例外：它是 agent.task_summary 的任务标题。
        # 例外必须精确到"数据类型 + 字段位置"，否则"允许任务标题"会顺带
        # 放行窗口标题——那正是这里要防的东西。
        if "window_title" in context or "window_title" in data:
            problems.append("含窗口标题字段")
        if "title" in context:
            problems.append("context 含窗口标题字段")
        if "title" in data and not is_task_summary:
            problems.append("data.title 只允许出现在 agent.task_summary 事件中")

        return problems
