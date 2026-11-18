"""test_flow 纯函数单测（宿主可跑；编排部分在网内验收覆盖）。

parse_summary 解析 producer/consumer CLI 的 "summary: k=v ..." 行；
count_stages 已由 agent/reconcile/counter.py 吸收（day23 收口，用例移至
agent/tests/test_counter.py）；DEFAULT_SCENARIO_ASSERTIONS 为 day25 指标断言默认集。
"""

import pytest

from agent.reconcile.metrics import compute_metrics, evaluate_assertions
from lab.scenarios.normal_delivery.test_flow import (
    DEFAULT_SCENARIO_ASSERTIONS,
    parse_summary,
    resolve_assertions,
)

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


def test_default_assertions_pass_on_perfect_metrics():
    """day25 默认断言集：对完美指标（万条验收口径）恰 PASS。"""
    perfect_counts = {
        "sent": 10000, "confirmed": 10000, "delivered": 10000,
        "processed_unique": 10000, "acked": 10000, "dup_business": 0,
    }

    metrics = compute_metrics(perfect_counts)
    result = evaluate_assertions(metrics, list(DEFAULT_SCENARIO_ASSERTIONS))

    assert metrics["processed_rate"] == 1.0  # 10000/10000 二进制精确
    assert result["verdict"] == "PASS"
    assert {d["metric"] for d in result["details"]} == {  # 顺序无语义（day25 评审 Rec）
        "processed_rate", "dup_rate", "loss_rate",
    }


def test_resolve_assertions_four_paths():
    """day25 评审 I-1+M-2：缺省/None → 默认集；**空列表保留**（vacuous 契约）；显式列表直通。"""
    # 缺省键 → 默认集
    assert resolve_assertions({}) == list(DEFAULT_SCENARIO_ASSERTIONS)
    # 显式 None → 默认集
    assert resolve_assertions({"assertions": None}) == list(DEFAULT_SCENARIO_ASSERTIONS)
    # 显式空列表 → 保留（"无断言"是合法输入，evaluate 层 vacuous PASS）
    assert resolve_assertions({"assertions": []}) == []
    # 显式列表 → 直通不改
    custom = [{"metric": "dup_rate", "op": "<=", "value": 0.01}]
    assert resolve_assertions({"assertions": custom}) is custom
