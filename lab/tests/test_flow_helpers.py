"""test_flow 纯函数单测（宿主可跑；编排部分在网内验收覆盖）。

parse_summary 解析 producer/consumer CLI 的 "summary: k=v ..." 行；
count_stages 已由 agent/reconcile/counter.py 吸收（day23 收口，用例移至
agent/tests/test_counter.py）。
"""

import pytest

from lab.scenarios.normal_delivery.test_flow import parse_summary

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


def test_parse_summary_missing_field_raises():
    output = "summary: sent=5 failed=0\n"  # 缺 confirmed
    with pytest.raises(ValueError, match="confirmed"):
        parse_summary(output, required=("sent", "confirmed", "failed"))


def test_parse_summary_keeps_non_int_values_as_str():
    """consumer 的 stopped=max 是字符串值，原样保留（day22 评审 M-5 顺带钉住）。"""
    output = "summary: delivered=3 processed=3 acked=3 requeued=0 stopped=max\n"
    result = parse_summary(output, required=("delivered", "acked", "stopped"))
    assert result["stopped"] == "max"
    assert result["acked"] == 3
