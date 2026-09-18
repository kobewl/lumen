"""Git 传感器：只扫描显式配置的工作目录，只上报元数据。

严格边界：
- 只在配置的 repo_roots 下查找仓库，绝不递归扫描整个磁盘；
- 只读取仓库名、分支、HEAD commit、commit message 首行、变更文件数；
- 不读取或上传文件正文、diff、remote URL、绝对路径；
- 每 60 秒轮询一次，仅状态变化时产生事件。

实现上直接调用 git 命令而不是引入 GitPython，减少依赖并保证行为可预期。
所有 git 调用都设置了超时，避免在异常仓库上卡住。
"""

from __future__ import annotations

import logging
import os
import subprocess
from datetime import datetime, timezone
from pathlib import Path

from ..config import Config
from ..event import Event
from ..privacy import PrivacyFilter
from ..ulid import new as new_ulid
from .base import Sensor

logger = logging.getLogger(__name__)

# git 命令超时（秒）。单个仓库异常不能拖慢整体轮询。
GIT_TIMEOUT = 10

# 扫描深度上限，避免在超深目录树上耗时。
MAX_SCAN_DEPTH = 3


class GitSensor(Sensor):
    """Git 仓库元数据采集。"""

    name = "git"

    def __init__(self, config: Config, privacy: PrivacyFilter, emit):
        super().__init__(emit)
        self.config = config
        self.privacy = privacy
        # 记录每个仓库上一次的状态，只在变化时上报。
        self._last_state: dict[str, tuple[str, str]] = {}
        self._repos: list[Path] = []

    def _run(self) -> None:
        self._repos = self._discover_repos()
        if not self._repos:
            logger.warning("未在 repo_roots 中发现 Git 仓库: %s", self.config.repo_roots)
            return

        logger.info("发现 %d 个 Git 仓库", len(self._repos))
        poll = max(10.0, self.config.git_poll_seconds)

        while not self.should_stop():
            try:
                for repo in self._repos:
                    if self.should_stop():
                        break
                    self._check_repo(repo)
            except Exception:
                logger.exception("Git 传感器轮询失败")

            if self._stop_event.wait(poll):
                break

    def _discover_repos(self) -> list[Path]:
        """在配置的根目录下查找 Git 仓库。

        广度优先、限制深度，避免在庞大的目录树上耗时。
        """
        found: list[Path] = []
        for root in self.config.repo_roots:
            root_path = Path(root).expanduser()
            if not root_path.is_dir():
                logger.warning("repo_root 不存在或不是目录: %s", root_path)
                continue
            if (root_path / ".git").exists():
                found.append(root_path)
                continue

            queue: list[tuple[Path, int]] = [(root_path, 0)]
            while queue:
                current, depth = queue.pop(0)
                if depth >= MAX_SCAN_DEPTH:
                    continue
                try:
                    entries = sorted(current.iterdir(), key=lambda p: p.name)
                except (PermissionError, OSError):
                    continue
                for entry in entries:
                    if not entry.is_dir() or entry.name.startswith("."):
                        # 跳过隐藏目录与常见的依赖目录。
                        continue
                    if entry.name in ("node_modules", "venv", ".venv", "target", "dist", "build"):
                        continue
                    if (entry / ".git").exists():
                        found.append(entry)
                    else:
                        queue.append((entry, depth + 1))
        return found

    def _run_git(self, repo: Path, *args: str) -> str:
        """执行 git 命令并返回 stdout；失败时返回空字符串。"""
        try:
            result = subprocess.run(
                ["git", "-C", str(repo), *args],
                capture_output=True, text=True, timeout=GIT_TIMEOUT, check=False,
                # 阻止 git 读取用户级 hooks 或提示交互。
                env={**os.environ, "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"},
            )
        except (subprocess.TimeoutExpired, OSError) as exc:
            logger.warning("git 命令失败 repo=%s: %s", repo.name, exc)
            return ""
        if result.returncode != 0:
            return ""
        return result.stdout.strip()

    def _check_repo(self, repo: Path) -> None:
        """检查单个仓库的状态变化。"""
        repo_name = repo.name
        branch = self._run_git(repo, "rev-parse", "--abbrev-ref", "HEAD")
        head = self._run_git(repo, "rev-parse", "HEAD")
        if not head:
            # 空仓库或损坏仓库，跳过。
            return

        previous = self._last_state.get(repo_name)
        now = datetime.now(timezone.utc)

        if previous is None:
            # 首次见到该仓库：只静默记录基线，不产生事件。
            #
            # 设计文档要求"仅状态变化时生成事件"，而首次扫描只是在建立基线，
            # 并不是变化。如果这里发事件，那么每次进程重启都会对所有仓库
            # 各发一条 workspace 事件，造成持续的噪声（实测每次重启 4 条）。
            self._last_state[repo_name] = (branch, head)
            return

        prev_branch, prev_head = previous
        if head == prev_head and branch == prev_branch:
            return  # 无变化，不上报

        self._last_state[repo_name] = (branch, head)

        if head != prev_head:
            # HEAD 变化通常意味着有新提交，取出提交信息（只取首行并脱敏）。
            message = self._run_git(repo, "log", "-1", "--pretty=%s", head)
            message = self.privacy.sanitize_text(message, limit=200)
            changed = self._count_changed_files(repo, prev_head, head)
            self._emit_git(
                repo_name, now, kind="commit", branch=branch,
                head=head, message=message, changed_files=changed,
            )
        else:
            # 只是切换分支。
            self._emit_git(repo_name, now, kind="workspace", branch=branch, head=head)

    def _count_changed_files(self, repo: Path, old_head: str, new_head: str) -> int:
        """统计两个 commit 之间变更的文件数。只取数量，不读内容。"""
        output = self._run_git(repo, "diff", "--name-only", old_head, new_head)
        if not output:
            return 0
        return len([line for line in output.splitlines() if line.strip()])

    def _emit_git(self, repo: str, when: datetime, kind: str, branch: str = "",
                  head: str = "", message: str = "", changed_files: int = 0) -> None:
        """构造并产出一条 git.activity 事件。"""
        # 项目名优先用白名单映射，未配置时直接用仓库名。
        project = self.config.project_keywords.get(repo, [None])[0] or repo
        event = Event.git_activity(
            event_id=new_ulid(),
            device_id=self.config.device_id,
            when=when,
            repo=repo,
            kind=kind,
            branch=branch,
            head_commit=head,
            commit_message=message,
            changed_files_count=changed_files,
            project=project if project != repo else None,
        )
        self.emit(event)

    def repos(self) -> list[str]:
        """返回发现的仓库名，供 status 命令展示。"""
        return [p.name for p in self._repos]
