"""ULID 生成与解析。

事件协议要求 event_id 使用客户端生成的 ULID。这里不引入第三方依赖，
直接用标准库实现，保持桌面端依赖最小（只需要 pyobjc）。

格式：48 位毫秒时间戳 + 80 位随机数，Crockford Base32 编码为 26 个字符。
"""

from __future__ import annotations

import os
import threading
import time

# Crockford Base32 字符集，去掉了容易混淆的 I、L、O、U。
ENCODING = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
LENGTH = 26

_lock = threading.Lock()
_last_ms = -1
_last_random = bytearray(10)


def _encode(raw: bytes) -> str:
    """把 16 字节数据编码成 26 位 Crockford Base32。

    16 字节是 128 位，26 个字符是 130 位，因此在最前面补 2 个 0 位。
    """
    bits = "00" + "".join(f"{b:08b}" for b in raw)
    return "".join(ENCODING[int(bits[i : i + 5], 2)] for i in range(0, 130, 5))


def _next_random() -> bytes:
    """同一毫秒内递增随机数，保证 ULID 单调递增且不重复。"""
    for i in range(len(_last_random) - 1, -1, -1):
        if _last_random[i] == 0xFF:
            _last_random[i] = 0
            continue
        _last_random[i] += 1
        return bytes(_last_random)
    # 全部溢出时重新随机，概率极低。
    _last_random[:] = os.urandom(10)
    return bytes(_last_random)


def new() -> str:
    """生成一个 ULID。"""
    return new_at(time.time())


def new_at(timestamp: float) -> str:
    """按指定时间戳生成 ULID（用于测试与假事件）。"""
    global _last_ms
    ms = int(timestamp * 1000)

    with _lock:
        if ms == _last_ms:
            rand = _next_random()
        else:
            _last_ms = ms
            _last_random[:] = os.urandom(10)
            rand = bytes(_last_random)

    ts_bytes = ms.to_bytes(6, "big")
    return _encode(ts_bytes + rand)


def is_valid(value: str) -> bool:
    """判断字符串是否是合法 ULID。"""
    if not isinstance(value, str) or len(value) != LENGTH:
        return False
    return all(ch in ENCODING for ch in value)


def timestamp_of(value: str) -> float:
    """解析 ULID 中的时间戳，返回 Unix 秒。非法输入抛 ValueError。"""
    if not is_valid(value):
        raise ValueError(f"非法 ULID: {value!r}")
    ms = 0
    for ch in value[:10]:
        ms = (ms << 5) | ENCODING.index(ch)
    return ms / 1000.0
