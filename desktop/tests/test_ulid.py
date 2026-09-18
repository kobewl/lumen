"""ULID 实现测试。

关键点是与官方规范兼容：同一毫秒内不重复、时间可解析、字符集合法。
"""

from __future__ import annotations

import time

import pytest

from lumen_desktop import ulid


def test_generated_is_valid():
    value = ulid.new()
    assert len(value) == 26
    assert ulid.is_valid(value)


def test_matches_official_spec_vector():
    """对照标准 ULID 规范：时间戳 1469918176385、随机数全 0 应为 01ARYZ6S41..."""
    rand = bytes(10)
    raw = (1469918176385).to_bytes(6, "big") + rand
    encoded = ulid._encode(raw)
    assert encoded.startswith("01ARYZ6S41"), encoded


def test_unique_within_same_millisecond():
    timestamp = time.time()
    values = {ulid.new_at(timestamp) for _ in range(2000)}
    assert len(values) == 2000


def test_monotonic_within_same_millisecond():
    timestamp = time.time()
    values = [ulid.new_at(timestamp) for _ in range(50)]
    assert values == sorted(values)


def test_timestamp_round_trip():
    timestamp = time.time()
    value = ulid.new_at(timestamp)
    parsed = ulid.timestamp_of(value)
    # ULID 精度是毫秒，允许 1ms 误差。
    assert abs(parsed - timestamp) < 0.002


def test_valid_rejects_bad_input():
    assert not ulid.is_valid("")
    assert not ulid.is_valid("01J9Z4QK7M3F8N2P5R7T9V1X3")     # 25 位
    assert not ulid.is_valid("01J9Z4QK7M3F8N2P5R7T9V1X3BB")   # 27 位
    assert not ulid.is_valid("01J9Z4QK7M3F8N2P5R7T9V1X3I")    # 含非法字符 I
    assert not ulid.is_valid("01J9Z4QK7M3F8N2P5R7T9V1X3l")    # 小写非法


def test_timestamp_of_rejects_invalid():
    with pytest.raises(ValueError):
        ulid.timestamp_of("not-a-ulid")


def test_golden_values_are_valid():
    """协议 golden 样例中的 id 必须通过本地校验，否则两端无法互通。"""
    for value in (
        "01J9Z4QK7M3F8N2P5R7T9V1X3B",
        "01J9Z4QK7M3F8N2P5R7T9V1X3C",
        "01J9Z4QK7M3F8N2P5R7T9V1X3D",
        "01J9Z5A2K4M6P8R0T2V4X6Z8B0",
    ):
        assert ulid.is_valid(value), value
