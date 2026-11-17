"""metrics 五指标与断言判定单测（纯函数；第 24 天验收=边界样本不崩、判定正确）。

口径出处：实施规划 §5/表 22/演示主线（处理率/重复率/恢复时间）；
day23 I-4 契约（时间维度 metrics 另读 events ts/故障时间线）。
"""

from datetime import UTC, datetime

import pytest
from agent.reconcile.metrics import compute_metrics, evaluate_assertions

T0 = "2026-11-18T10:00:00+00:00"


def ts(offset_s: float) -> str:
    base = datetime.fromisoformat(T0)
    return datetime.fromtimestamp(base.timestamp() + offset_s, tz=UTC).isoformat()


def ev(mid, event, offset_s):
    return {"message_id": mid, "event": event, "ts": ts(offset_s)}


def counts(**kw):
    base = {"sent": 10, "confirmed": 10, "delivered": 10, "processed_unique": 10,
            "acked": 10, "dup_business": 0}
    base.update(kw)
    return base


def test_metrics_normal_counts():
    result = compute_metrics(counts())

    assert result["processed_rate"] == 1.0
    assert result["dup_rate"] == 0.0
    assert result["loss_rate"] == 0.0
    assert result["avg_delivery_latency_s"] is None  # 未提供 events
    assert result["recovery_time_s"] is None  # 未提供 fault


def test_metrics_partial_values():
    result = compute_metrics(counts(sent=10, acked=8, processed_unique=8, dup_business=2))

    assert result["processed_rate"] == pytest.approx(0.8)
    assert result["dup_rate"] == pytest.approx(2 / 10)  # 2 / (8+2) 总处理次数
    assert result["loss_rate"] == pytest.approx(0.2)


def test_metrics_zero_denominators_return_none():
    """验收原文边界①：0 分母不崩，返回 None（不撒谎）。"""
    empty = compute_metrics(counts(sent=0, acked=0, processed_unique=0, dup_business=0))

    assert empty["processed_rate"] is None
    assert empty["loss_rate"] is None
    assert empty["dup_rate"] is None  # 总处理次数 0


def test_metrics_all_lost():
    """验收原文边界②：全丢失（发 n 一条未 ack）。"""
    result = compute_metrics(counts(sent=10, acked=0, delivered=0, processed_unique=0))

    assert result["loss_rate"] == 1.0
    assert result["processed_rate"] == 0.0  # 分母非 0：精确 0
    assert result["dup_rate"] is None


def test_latency_from_events():
    events = [
        ev("m-1", "SEND", 0), ev("m-1", "ACKED", 2.0),
        ev("m-2", "SEND", 1.0), ev("m-2", "ACKED", 5.0),
    ]

    result = compute_metrics(counts(), events=events)

    assert result["avg_delivery_latency_s"] == pytest.approx(3.0)  # (2+4)/2


def test_latency_excludes_incomplete_ids():
    events = [
        ev("m-1", "SEND", 0), ev("m-1", "ACKED", 2.0),
        ev("m-2", "SEND", 0),  # 未 ACKED：排除
        ev("m-3", "ACKED", 9.0),  # 无 SEND：排除
    ]

    result = compute_metrics(counts(), events=events)

    assert result["avg_delivery_latency_s"] == pytest.approx(2.0)

    only_incomplete = [ev("m-2", "SEND", 0)]
    assert compute_metrics(counts(), events=only_incomplete)["avg_delivery_latency_s"] is None


def test_latency_malformed_ts_fails_fast():
    bad = [{"message_id": "m-1", "event": "SEND", "ts": "not-a-time"},
           ev("m-1", "ACKED", 1)]

    with pytest.raises(ValueError, match="m-1"):
        compute_metrics(counts(), events=bad)


def test_recovery_time_from_fault():
    fault = {"injected_at": T0, "recovered_at": ts(17.5)}

    result = compute_metrics(counts(), fault=fault)

    assert result["recovery_time_s"] == pytest.approx(17.5)
    assert compute_metrics(counts())["recovery_time_s"] is None


# ---------- evaluate_assertions ----------


def _metrics(**overrides):
    base = {
        "processed_rate": 1.0,
        "dup_rate": 0.0,
        "loss_rate": 0.0,
        "avg_delivery_latency_s": 3.0,
        "recovery_time_s": None,
    }
    base.update(overrides)
    return base


def test_assertion_op_matrix():
    metrics = _metrics(processed_rate=0.99, dup_rate=0.01)

    passing = [
        {"metric": "processed_rate", "op": ">=", "value": 0.99},
        {"metric": "processed_rate", "op": "==", "value": 0.99},
        {"metric": "dup_rate", "op": "<=", "value": 0.01},
        {"metric": "dup_rate", "op": "<", "value": 0.02},
        {"metric": "processed_rate", "op": ">", "value": 0.98},
    ]
    result = evaluate_assertions(metrics, passing)
    assert result["verdict"] == "PASS"
    assert all(d["passed"] for d in result["details"])

    failing = [
        {"metric": "processed_rate", "op": ">=", "value": 0.995},
        {"metric": "processed_rate", "op": "==", "value": 0.98},
        {"metric": "dup_rate", "op": "<=", "value": 0.005},
        {"metric": "dup_rate", "op": "<", "value": 0.01},
        {"metric": "processed_rate", "op": ">", "value": 0.99},
    ]
    result = evaluate_assertions(metrics, failing)
    assert result["verdict"] == "FAIL"
    assert not any(d["passed"] for d in result["details"])


def test_assertion_none_metric_fails_with_reason():
    """验收原文核心：0 分母的 None 指标遇断言 → FAIL+reason，非 vacuous pass。"""
    result = evaluate_assertions(
        _metrics(processed_rate=None),
        [{"metric": "processed_rate", "op": ">=", "value": 0.99}],
    )

    assert result["verdict"] == "FAIL"
    detail = result["details"][0]
    assert detail["passed"] is False
    assert "undefined" in detail["reason"]
    assert detail["actual"] is None


def test_unknown_op_fails_fast():
    with pytest.raises(ValueError, match="op"):
        evaluate_assertions(_metrics(), [{"metric": "dup_rate", "op": "~=", "value": 1}])


def test_unknown_metric_fails_fast():
    with pytest.raises(ValueError, match="nonexistent_metric"):
        evaluate_assertions(_metrics(), [{"metric": "nonexistent_metric", "op": ">=", "value": 1}])


def test_verdict_aggregation_any_fail_is_fail():
    assertions = [
        {"metric": "processed_rate", "op": ">=", "value": 0.99},
        {"metric": "dup_rate", "op": "==", "value": 0.0},
        {"metric": "loss_rate", "op": "<=", "value": -1},  # 唯一 FAIL
    ]
    result = evaluate_assertions(_metrics(), assertions)

    assert result["verdict"] == "FAIL"
    assert [d["passed"] for d in result["details"]] == [True, True, False]


def test_empty_assertions_pass_vacuously():
    """空 assertions → PASS（vacuous，显式口径：无可失败项）。"""
    result = evaluate_assertions(_metrics(), [])

    assert result["verdict"] == "PASS"
    assert result["details"] == []


# ---------- day24 评审处置：I-1 / I-2 / M-3 / M-4 / M-5 ----------


def test_probe_rows_excluded_from_latency():
    """评审 I-1：探针行（含 SEND/ACKED 齐全形态）不参与 latency——与 counter 契约对称。"""
    from lab.helpers.events import PROBE_PREFIX

    events = [
        ev("m-1", "SEND", 0), ev("m-1", "ACKED", 2.0),
        ev(f"{PROBE_PREFIX}x", "SEND", 0), ev(f"{PROBE_PREFIX}x", "ACKED", 600.0),
    ]

    result = compute_metrics(counts(), events=events)

    assert result["avg_delivery_latency_s"] == pytest.approx(2.0)  # 600s 探针被排除


def test_rate_out_of_range_fails_with_pollution_reason():
    """评审 I-2：物理不可能值（负丢失率/处理率>1）→ FAIL+污染 reason，不被洗白。"""
    polluted = _metrics(loss_rate=-0.6)

    result = evaluate_assertions(
        polluted, [{"metric": "loss_rate", "op": "<=", "value": 0.05}]
    )

    assert result["verdict"] == "FAIL"
    assert "out of range" in result["details"][0]["reason"]

    over = _metrics(processed_rate=1.6)
    result = evaluate_assertions(over, [{"metric": "processed_rate", "op": ">=", "value": 0.99}])
    assert result["verdict"] == "FAIL"


def test_string_value_and_missing_metric_key_fail_fast():
    """评审 M-3：value 非数值（TypeError）/ metrics 缺键（ValueError）→ 点名。"""
    with pytest.raises(TypeError, match="value"):
        evaluate_assertions(_metrics(), [{"metric": "dup_rate", "op": "<=", "value": "0.1"}])
    partial = {"dup_rate": 0.0}
    with pytest.raises(ValueError, match="loss_rate"):
        evaluate_assertions(partial, [{"metric": "loss_rate", "op": "<=", "value": 0.1}])


def test_fault_missing_key_fails_fast():
    """评审 M-4：fault 半缺键 → ValueError 点名（非裸 KeyError）。"""
    with pytest.raises(ValueError, match="recovered_at"):
        compute_metrics(counts(), fault={"injected_at": T0})


def test_events_row_missing_key_fails_fast():
    """评审 M-5：events 行缺键 → ValueError 点名下标（与 counter 同体感）。"""
    bad = [{"message_id": "m-1", "event": "SEND"}]  # 缺 ts

    with pytest.raises(ValueError, match="index 0"):
        compute_metrics(counts(), events=bad)
