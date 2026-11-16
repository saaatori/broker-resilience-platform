"""test_flow 纯函数单测（宿主可跑；编排部分在网内验收覆盖）。

parse_summary 解析 producer/consumer CLI 的 "summary: k=v ..." 行；
count_stages 是 day23 counter.py 正式化前的场景轻量版（含 fixture-probe- 排除契约）。
"""

import pytest

from lab.scenarios.normal_delivery.test_flow import count_stages, parse_summary

PRODUCER_OUTPUT = "\n".join(
    [
        "2026-11-16 [INFO] [SEND] seq=0 id=x",
        "2026-11-16 [INFO] [CONFIRM] seq=0 id=x",
        "2026-11-16 [INFO] summary: sent=1000 confirmed=1000 failed=0 recorder_misses=0",
    ]
)


def test_parse_summary_extracts_int_fields():
    assert parse_summary(PRODUCER_OUTPUT) == {
        "sent": 1000,
        "confirmed": 1000,
        "failed": 0,
        "recorder_misses": 0,
    }


def test_parse_summary_missing_line_raises():
    with pytest.raises(ValueError, match="summary line not found"):
        parse_summary("只有噪声\n没有汇总\n")


def test_parse_summary_keeps_non_int_values_as_str():
    """consumer 的 stopped=max 是字符串值，原样保留（评审 M-5 顺带钉住）。"""
    output = "summary: delivered=3 processed=3 acked=3 requeued=0 stopped=max\n"
    result = parse_summary(output, required=("delivered", "acked", "stopped"))
    assert result["stopped"] == "max"
    assert result["acked"] == 3


def test_parse_summary_missing_field_raises():
    output = "summary: sent=5 failed=0\n"  # 缺 confirmed
    with pytest.raises(ValueError, match="confirmed"):
        parse_summary(output, required=("sent", "confirmed", "failed"))


def _events(n, *, with_probe=False, incomplete_id=None):
    events = []
    for i in range(n):
        mid = f"m-{i}"
        for event in ("SEND", "CONFIRM", "DELIVERED", "PROCESSED", "ACKED"):
            if incomplete_id == mid and event == "ACKED":
                continue
            events.append({"message_id": mid, "event": event, "ts": "t"})
    if with_probe:
        events.append({"message_id": "fixture-probe-1", "event": "DELIVERED", "ts": "t"})
    return events


def test_count_stages_excludes_probe_prefix():
    result = count_stages(_events(100, with_probe=True))

    assert result["counts"] == {
        "SEND": 100, "CONFIRM": 100, "DELIVERED": 100, "PROCESSED": 100, "ACKED": 100,
    }
    assert result["total_ids"] == 100  # 探针 id 不计入
    assert result["incomplete_ids"] == []


def test_count_stages_reports_incomplete_ids():
    result = count_stages(_events(3, incomplete_id="m-1"))

    assert result["incomplete_ids"] == ["m-1"]  # 缺 ACKED 被点名
    assert result["counts"]["ACKED"] == 2
