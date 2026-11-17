"""counter 五阶段计数单测（纯函数 + JSONL fixture 文件——第 23 天验收原文）。

口径出处：实施规划 §5（五阶段+阶段集合差）；day20 口径（DUP 仅 PROCESSED 重复）；
day21 契约（unique(message_id,event)、probe 前缀排除）；day22 评审三增量
（权威面=CLI summary 覆盖 / 证据面=JSONL 差集 / 吸收不重写）。
"""

import json
from pathlib import Path

import pytest
from agent.reconcile.counter import count_stages
from lab.helpers.events import (
    EVENT_ACKED,
    EVENT_CONFIRM,
    EVENT_DELIVERED,
    EVENT_PROCESSED,
    EVENT_SEND,
    PROBE_PREFIX,
)

FIXTURE = Path(__file__).parent / "fixtures" / "reconcile_sample.jsonl"


def load_fixture() -> list:
    return [json.loads(line) for line in FIXTURE.read_text(encoding="utf-8").splitlines() if line]


def msg(mid, events):
    return [{"message_id": mid, "event": e, "ts": "t"} for e in events]


CLEAN = [EVENT_SEND, EVENT_CONFIRM, EVENT_DELIVERED, EVENT_PROCESSED, EVENT_ACKED]


def test_clean_sample():
    events = msg("m-1", CLEAN) + msg("m-2", CLEAN)

    result = count_stages(events)

    assert (result["sent"], result["confirmed"], result["delivered"]) == (2, 2, 2)
    assert (result["processed_unique"], result["acked"], result["dup_business"]) == (2, 2, 0)
    assert result["dup_business_ids"] == []
    for key in (
        "sent_not_confirmed",
        "confirmed_not_delivered",
        "delivered_not_processed",
        "processed_not_acked",
    ):
        assert result[key] == []


def test_fixture_file_counts():
    """验收原文：构造含丢失/重复样本的 fixture，计数全部正确。"""
    result = count_stages(load_fixture())

    assert (result["sent"], result["confirmed"], result["delivered"]) == (12, 11, 10)
    assert (result["processed_unique"], result["acked"], result["dup_business"]) == (9, 8, 1)
    assert result["dup_business_ids"] == ["m-11"]
    assert result["sent_not_confirmed"] == ["m-07"]
    assert result["confirmed_not_delivered"] == ["m-08"]
    assert result["delivered_not_processed"] == ["m-09"]
    assert result["processed_not_acked"] == ["m-10"]


def test_dup_only_from_processed_repeat():
    # 重复 DELIVERED/ACKED（重投，day20 口径）不计 dup；重复 PROCESSED 计
    redelivered = msg("m-r", ["SEND", "CONFIRM", "DELIVERED", "DELIVERED", "PROCESSED", "ACKED"])
    assert count_stages(redelivered)["dup_business"] == 0

    dup = msg("m-d", CLEAN + ["PROCESSED", "DUP_BUSINESS"])
    result = count_stages(dup)
    assert result["dup_business"] == 1
    assert result["dup_business_ids"] == ["m-d"]


def test_stage_diffs_direction():
    # 单消息逐段缺：恰落入对应差集（差集=前阶段 unique 集 − 后阶段 unique 集）
    assert count_stages(msg("a", ["SEND"]))["sent_not_confirmed"] == ["a"]
    assert count_stages(msg("b", CLEAN[:2]))["confirmed_not_delivered"] == ["b"]
    assert count_stages(msg("c", CLEAN[:3]))["delivered_not_processed"] == ["c"]
    assert count_stages(msg("d", CLEAN[:4]))["processed_not_acked"] == ["d"]


def test_producer_summary_overrides():
    """口径②（day22 增量）：recorder 熔断丢 SEND 行时，权威计数取 producer summary。"""
    events = msg("m-1", CLEAN) + msg("m-2", CLEAN[:1])  # JSONL 里 SEND 只有 2 行、CONFIRM 1 行
    producer_summary = {"sent": 1000, "confirmed": 998}

    result = count_stages(events, producer_summary=producer_summary)

    assert (result["sent"], result["confirmed"]) == (1000, 998)  # 权威面
    assert result["sent_not_confirmed"] == ["m-2"]  # 证据面仍按 JSONL 行


def test_consumer_summary_overrides_acked_only():
    """评审 I-2 裁决：consumer 的 delivered/processed 是尝试口径（重投再加一），
    与 unique 字段语义不符——仅 acked（每消息至多一次）可被 summary 覆盖。"""
    events = msg("m-1", CLEAN[:3])  # JSONL 缺 PROCESSED/ACKED 行（recorder 丢事件）
    consumer_summary = {"delivered": 10, "processed": 10, "acked": 10}

    result = count_stages(events, consumer_summary=consumer_summary)

    assert result["acked"] == 10  # 语义匹配 → 覆盖
    assert result["delivered"] == 1 and result["processed_unique"] == 0  # 尝试口径不污染 unique
    assert result["delivered_not_processed"] == ["m-1"]  # 证据面


def test_probe_prefix_excluded():
    events = msg(f"{PROBE_PREFIX}x", [EVENT_DELIVERED, EVENT_PROCESSED]) + msg("m-1", CLEAN)

    result = count_stages(events)

    assert result["sent"] == 1 and result["delivered"] == 1  # 探针行不计
    assert result["total_events"] == 5 + 2  # 原始行数（含探针）透明披露


def test_exclude_prefix_empty_string_disables_filtering():
    """评审 M-7：空串=显式关闭排除（哨兵语义钉死），不是"全排除"。"""
    events = msg(f"{PROBE_PREFIX}x", [EVENT_DELIVERED]) + msg("m-1", CLEAN)

    assert count_stages(events, exclude_prefix="")["delivered"] == 2


def test_malformed_row_names_index():
    """评审 M-8：畸形行点名下标 fail-fast（不裸 KeyError）。"""
    bad = [{"message_id": "m-1", "event": EVENT_SEND, "ts": "t"}, {"event": "SEND"}]

    with pytest.raises(ValueError, match="index 1"):
        count_stages(bad)


def test_empty_events():
    result = count_stages([])

    assert result["sent"] == 0 and result["dup_business"] == 0
    assert result["dup_business_ids"] == []
