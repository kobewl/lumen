"""批量同步客户端。

同步语义：at-least-once 投递 + 服务端按 event_id 幂等。
- 正常每 5 分钟一次；队列达到 100 条或 512KB 时立即同步；
- 失败退避：5 秒、15 秒、1 分钟、5 分钟、15 分钟封顶；
- 只按服务端逐条结果更新状态，绝不使用“最大 ULID 游标”批量确认。

网络访问只发生在这个模块；传感器不得直接联网。
"""

from __future__ import annotations

import gzip
import json
import logging
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from .config import Config
from .event import Event
from .privacy import PrivacyFilter
from .storage import Storage
from .ulid import new as new_ulid

logger = logging.getLogger(__name__)

# 退避阶梯（秒），超出后取最后一个值。
BACKOFF_SECONDS = [5, 15, 60, 300, 900]

# 服务器返回的可重试错误码。
RETRYABLE_ERROR_CODES = {"storage_error", "internal_error"}


@dataclass
class SyncResult:
    """一次同步尝试的结果。"""

    attempted: int = 0
    accepted: int = 0
    duplicate: int = 0
    rejected: int = 0
    retried: int = 0
    skipped: bool = False
    # blocked 表示这次「没做成」是真实阻塞（未注册、队列硬上限、事件被拒），
    # 而不是无事可做（队列为空）。调用方据此决定是否保留错误提示：
    # 阻塞要留在 status 里提醒用户，无事可做则应清掉过期的错误。
    blocked: bool = False
    message: str = ""
    rejected_details: list[dict[str, Any]] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.skipped and self.rejected == 0 and self.retried == 0


class SyncClient:
    """负责把本地事件批量上传到 lumen-server。"""

    def __init__(self, config: Config, storage: Storage, token: str,
                 privacy: PrivacyFilter | None = None):
        self.config = config
        self.storage = storage
        self.token = token
        self.privacy = privacy or PrivacyFilter(config.project_keywords)

    # ---- 主流程 ----

    def sync_once(self, force: bool = False) -> SyncResult:
        """执行一次同步尝试。

        force=True 时忽略“达到阈值”判断，直接尝试上传（用于 sync-now 与手动重试）。
        """
        if not self.token:
            return SyncResult(skipped=True, blocked=True, message="未注册设备，缺少 device_token")
        if self._hard_limit_reached() and not force:
            return SyncResult(skipped=True, blocked=True, message="本地队列达到硬上限，已暂停新采集")

        events = self.storage.pending_events(
            limit=self.config.sync_batch_max_events,
            max_bytes=self.config.sync_batch_max_bytes,
        )
        if not events:
            # 「队列为空」和「事件正在退避等待」是两种不同状态：前者无事可做，
            # 后者说明上一次同步失败、还有数据没送出去，必须区分开，
            # 否则 status 会把「还有 5 条在等重试」显示成「没有待同步事件」。
            waiting = self._waiting_count()
            if waiting > 0:
                return SyncResult(
                    skipped=True, blocked=True,
                    message=f"{waiting} 条事件等待重试（上次同步失败，稍后自动重试）",
                )
            return SyncResult(skipped=True, message="没有待同步事件")

        batch_id = new_ulid()
        # 上传前最后一道隐私检查：任何一条不通过就拒绝上传并标记。
        #
        # 末地防线拦下的事件等于数据没上报，必须计入 rejected 并在 status 里可见；
        # 静默跳过会让用户以为一切正常，而实际上事件已经被丢弃。
        safe_events: list[Event] = []
        rejected_local: list[dict[str, Any]] = []
        for event in events:
            problems = self.privacy.validate_payload(event.to_payload())
            if problems:
                reason = "隐私校验未通过: " + "; ".join(problems)
                self.storage.mark_rejected(event.id, reason)
                logger.warning("事件 %s 未通过隐私校验: %s", event.id, problems)
                rejected_local.append({"event_id": event.id, "code": "privacy_check",
                                       "message": reason})
                continue
            safe_events.append(event)

        if not safe_events:
            return SyncResult(
                attempted=len(events), rejected=len(rejected_local), skipped=True, blocked=True,
                message=f"本批 {len(rejected_local)} 条事件均未通过隐私校验，已标记为拒绝",
                rejected_details=rejected_local,
            )

        result = SyncResult(attempted=len(safe_events))
        try:
            response = self._post_batch(batch_id, safe_events)
        except _RetryableError as exc:
            self._handle_retry(safe_events, batch_id, str(exc))
            result.retried = len(safe_events)
            result.message = f"网络或服务端暂时不可用，已安排重试: {exc}"
            return result
        except _FatalError as exc:
            result.message = f"同步失败: {exc}"
            return result

        self._apply_response(safe_events, batch_id, response, result)
        return result

    # ---- HTTP ----

    def _post_batch(self, batch_id: str, events: list[Event]) -> dict[str, Any]:
        """发送批量请求，返回解析后的响应。"""
        body = {
            "device_id": self.config.device_id,
            "batch_id": batch_id,
            "sent_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "events": [e.to_payload() for e in events],
        }
        raw = json.dumps(body, ensure_ascii=False).encode("utf-8")

        # 超过 4KB 才值得压缩，小批量压缩反而增加开销。
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.token}",
            "User-Agent": "lumen-desktop/0.1.0",
        }
        if len(raw) > 4096:
            raw = gzip.compress(raw)
            headers["Content-Encoding"] = "gzip"

        url = self.config.server_url.rstrip("/") + "/api/v1/events/batch"
        request = urllib.request.Request(url, data=raw, headers=headers, method="POST")

        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                payload = response.read().decode("utf-8")
                return json.loads(payload)
        except urllib.error.HTTPError as exc:
            # 401/403 属于凭证或配置问题，重试没有意义。
            if exc.code in (401, 403):
                raise _FatalError(f"鉴权失败({exc.code})，请检查 device_token") from exc
            if exc.code == 413:
                raise _FatalError("批次过大，已被服务端拒绝") from exc
            if exc.code >= 500 or exc.code == 429:
                raise _RetryableError(f"服务端返回 {exc.code}") from exc
            raise _FatalError(f"服务端返回 {exc.code}") from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise _RetryableError(f"网络不可达: {exc}") from exc
        except json.JSONDecodeError as exc:
            raise _RetryableError("响应不是合法 JSON") from exc

    # ---- 响应处理 ----

    def _apply_response(self, events: list[Event], batch_id: str,
                        response: dict[str, Any], result: SyncResult) -> None:
        """按逐事件 ACK 更新本地状态。

        关键：只把 accepted/duplicate 标记为已同步，rejected 记录错误，
        retryable 保持待同步。绝不根据 batch_id 整体确认。
        """
        results = response.get("results")
        if not isinstance(results, list):
            self._handle_retry(events, batch_id, "响应缺少 results 字段")
            result.retried = len(events)
            result.message = "响应格式异常，已安排重试"
            return

        by_id = {item.get("event_id"): item for item in results if isinstance(item, dict)}
        known_ids = {e.id for e in events}

        synced_ids: list[str] = []
        retry_ids: list[str] = []

        for event in events:
            item = by_id.get(event.id)
            if item is None:
                # 服务端没有回应这条事件，必须重试，不能假定成功。
                retry_ids.append(event.id)
                continue

            status = item.get("status")
            if status in ("accepted", "duplicate"):
                # duplicate 是成功状态，不是错误。
                synced_ids.append(event.id)
                if status == "accepted":
                    result.accepted += 1
                else:
                    result.duplicate += 1
            elif status == "rejected":
                code = str(item.get("code", "unknown"))
                message = str(item.get("message", ""))
                if code in RETRYABLE_ERROR_CODES:
                    retry_ids.append(event.id)
                else:
                    result.rejected += 1
                    result.rejected_details.append(
                        {"event_id": event.id, "code": code, "message": message}
                    )
                    self.storage.mark_rejected(event.id, f"{code}: {message}")
            else:
                retry_ids.append(event.id)

        # 服务端返回了未在本次请求中的 id：属于异常响应，记录但不处理。
        unexpected = set(by_id) - known_ids
        if unexpected:
            logger.warning("响应包含未知 event_id: %d 条", len(unexpected))

        if synced_ids:
            self.storage.mark_synced(synced_ids, batch_id)
        if retry_ids:
            self._handle_retry([e for e in events if e.id in set(retry_ids)], batch_id, "服务端未确认")
            result.retried = len(retry_ids)

        if result.rejected:
            result.message = f"部分事件被永久拒绝: {result.rejected} 条"
        else:
            result.message = (
                f"同步完成: 新增 {result.accepted} 条, 重复 {result.duplicate} 条"
            )
        if synced_ids:
            self.storage.set_meta("last_sync_at", datetime.now(timezone.utc).isoformat())

    def _handle_retry(self, events: list[Event], batch_id: str, error: str) -> None:
        """按退避阶梯安排重试。"""
        if not events:
            return
        # 用这批事件里最大的尝试次数决定退避间隔，保证整批节奏一致。
        attempts = max((self.storage.attempts_for(e.id) for e in events), default=0)
        delay = BACKOFF_SECONDS[min(attempts, len(BACKOFF_SECONDS) - 1)]
        self.storage.schedule_retry([e.id for e in events], batch_id, delay, error)
        logger.info("已安排 %d 条事件在 %s 秒后重试", len(events), delay)

    def _hard_limit_reached(self) -> bool:
        """判断未同步数据是否达到硬上限；达到后暂停新采集。"""
        hard_limit = self.config.queue_hard_limit_mb * 1024 * 1024
        return self.storage.queue_size_bytes() >= hard_limit

    def _waiting_count(self) -> int:
        """返回仍在队列中（含等待退避重试）的事件数。"""
        try:
            return int(self.storage.counts().get("queued", 0))
        except Exception:
            logger.exception("查询队列状态失败")
            return 0

    def queue_status(self) -> dict[str, Any]:
        """返回队列状态，供 status 命令与阈值判断使用。"""
        size = self.storage.queue_size_bytes()
        counts = self.storage.counts()
        soft = self.config.queue_soft_limit_mb * 1024 * 1024
        hard = self.config.queue_hard_limit_mb * 1024 * 1024
        return {
            "queue_bytes": size,
            "queue_mb": round(size / 1024 / 1024, 2),
            "soft_limit_reached": size >= soft,
            "hard_limit_reached": size >= hard,
            "counts": counts,
            "last_sync_at": self.storage.get_meta("last_sync_at"),
            "last_error": self.storage.get_meta("last_sync_error"),
        }


class _RetryableError(RuntimeError):
    """可重试的错误：网络故障、5xx、429、响应格式异常。"""


class _FatalError(RuntimeError):
    """不可重试的错误：鉴权失败、请求过大等。"""
