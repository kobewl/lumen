"""macOS Keychain 封装：安全保存 device_token。

设计要点：
- device_token 只存在系统钥匙串，配置文件与数据库里都不出现；
- 优先使用 `security` 命令行工具，避免额外依赖；
- 没有 Keychain 的环境（如 CI）返回 None，由调用方决定降级策略。
"""

from __future__ import annotations

import logging
import subprocess

logger = logging.getLogger(__name__)

SERVICE_NAME = "com.lumen.desktop"
ACCOUNT_NAME = "device_token"


class KeychainError(RuntimeError):
    """钥匙串操作失败。"""


def _run_security(args: list[str], input_text: str | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["security", *args],
        input=input_text,
        capture_output=True,
        text=True,
        check=False,
    )


def store_token(token: str, account: str = ACCOUNT_NAME, service: str = SERVICE_NAME) -> bool:
    """把 token 写入钥匙串；已存在时先删除再写入。

    实现细节：`security add-generic-password -w` 不带值时会交互式索要密码，
    并且**要求输入两次**（密码 + 确认）。只喂一次会以 "passwords don't match"
    失败，而且它读取失败时仍可能返回 0，导致条目存在但值为空——这是一个很难
    发现的静默故障。因此这里明确输入两遍。

    为什么不用命令行参数传值：那会让 token 出现在进程列表（ps）里。
    """
    delete_token(account=account, service=service)
    result = _run_security(
        ["add-generic-password", "-a", account, "-s", service, "-U", "-w"],
        input_text=f"{token}\n{token}\n",
    )
    if result.returncode != 0:
        # 只记录返回码，不记录 stderr（可能包含 token 相关提示）。
        logger.error("写入钥匙串失败, returncode=%s", result.returncode)
        return False

    # 写回后校验一次，确保不是"条目存在但值为空"的静默失败。
    if load_token(account=account, service=service) != token:
        logger.error("钥匙串写入后校验失败：读取到的值不正确")
        return False
    return True


def load_token(account: str = ACCOUNT_NAME, service: str = SERVICE_NAME) -> str | None:
    """从钥匙串读取 token；不存在时返回 None。"""
    result = _run_security(["find-generic-password", "-a", account, "-s", service, "-w"])
    if result.returncode != 0:
        return None
    token = result.stdout.strip()
    return token or None


def delete_token(account: str = ACCOUNT_NAME, service: str = SERVICE_NAME) -> bool:
    """删除钥匙串中的 token；不存在时也视为成功。"""
    result = _run_security(["delete-generic-password", "-a", account, "-s", service])
    return result.returncode == 0 or "could not be found" in (result.stderr or "")


def has_token(account: str = ACCOUNT_NAME, service: str = SERVICE_NAME) -> bool:
    """判断钥匙串中是否已有 token。"""
    return load_token(account=account, service=service) is not None
