"""协议三 schema 校验测试（第 28 天验收=样例 JSON 通过 schema 校验；负样本真实红）。

schema 是协议 v1.0.1 委托的细化权威；本测试同时钉三处防漂移：
metric 枚举=metrics.METRIC_NAMES、params 样例值=params.default.json、substate 九值集。
"""

import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator, FormatChecker

from agent.reconcile.metrics import METRIC_NAMES

SCHEMA_DIR = Path(__file__).resolve().parents[2] / "docs" / "protocol" / "schema"
SAMPLES = SCHEMA_DIR / "samples"

SUBSTATES = ["CLAIMED", "PREPARING", "BASELINE", "FAULT_INJECTED",
             "RECOVERING", "VERIFYING", "UPLOADING", "DONE", "ABORTED"]  # 协议 §5 序列


def load(name: str) -> dict:
    return json.loads((SCHEMA_DIR / f"{name}.schema.json").read_text(encoding="utf-8"))


def load_sample(name: str) -> dict:
    return json.loads((SAMPLES / name).read_text(encoding="utf-8"))


def validate(schema: dict, doc) -> list[str]:
    """返回错误消息列表（空=通过）。"""
    validator = Draft202012Validator(schema, format_checker=FormatChecker())
    return [e.message for e in validator.iter_errors(doc)]


# ---------- 正样本（验收原文：样例 JSON 通过 schema 校验） ----------


def test_params_sample_passes():
    assert validate(load("params_snapshot"), load_sample("params.sample.json")) == []


def test_events_sample_passes_and_all_three_payload_types():
    schema = load("events")
    for name in ("events.sample.json", "events_log.sample.json", "events_progress.sample.json"):
        assert validate(schema, load_sample(name)) == [], name


def test_result_sample_passes():
    assert validate(load("result"), load_sample("result.sample.json")) == []


# ---------- 负样本（每 schema ≥3：缺必填/枚举外/类型错/oneOf 互斥） ----------


def test_params_negative_cases():
    schema = load("params_snapshot")
    good = load_sample("params.sample.json")

    bad = dict(good)
    del bad["idle_timeout_s"]  # 缺必填
    assert any("idle_timeout_s" in e for e in validate(schema, bad))

    bad = dict(good)
    bad["assertions"] = [{"metric": "nonexistent", "op": ">=", "value": 1}]  # 枚举外 metric
    assert validate(schema, bad)

    bad = dict(good)
    bad["assertions"] = [{"metric": "dup_rate", "op": ">=", "value": "0.1"}]  # value 类型错
    assert validate(schema, bad)

    bad = dict(good)
    bad["surprise_key"] = 1  # additionalProperties: false
    assert validate(schema, bad)


def test_events_negative_cases():
    schema = load("events")
    base = load_sample("events.sample.json")

    bad = dict(base)
    bad["payload"] = {"substate": "RUNNING"}  # 枚举外（协议无 RUNNING 子状态——day27 I-3 钉过）
    assert validate(schema, bad)

    bad = dict(base)
    bad["payload"] = {"substate": "PREPARING", "message": "混型"}  # 载荷内混型
    assert validate(schema, bad)

    # v1.0.3 回归：type 与 payload 形状绑定（day28 评审 I-1——曾放行）
    bad = {"seq": 1, "ts": base["ts"], "type": "LOG", "payload": {"substate": "DONE"}}
    assert validate(schema, bad)
    bad = {"seq": 1, "ts": base["ts"], "type": "STATUS", "payload": {"message": "x"}}
    assert validate(schema, bad)

    bad = dict(base)
    del bad["ts"]  # 缺必填
    assert validate(schema, bad)

    bad = dict(base)
    bad["ts"] = "not-a-time"  # format date-time
    assert validate(schema, bad)

    bad = dict(base)
    bad["seq"] = "1"  # 类型错（评审 M-6）
    assert validate(schema, bad)


def test_result_negative_cases():
    schema = load("result")
    good = load_sample("result.sample.json")

    bad = json.loads(json.dumps(good))
    bad["status"] = "TIMEOUT"  # 枚举外
    assert validate(schema, bad)

    bad = json.loads(json.dumps(good))
    del bad["reconciliation"]["total_events"]  # 嵌套缺必填
    assert validate(schema, bad)

    bad = json.loads(json.dumps(good))
    bad["summary"]["processed_rate"] = 1.6  # 率值域 [0,1]（越界=污染，day24 I-2 契约）
    assert validate(schema, bad)

    bad = json.loads(json.dumps(good))
    bad["attempt"] = "1"  # 类型错（评审 M-6）
    assert validate(schema, bad)

    bad = json.loads(json.dumps(good))
    bad["surprise"] = 1  # additionalProperties（评审 M-6）
    assert validate(schema, bad)

    bad = json.loads(json.dumps(good))
    bad["summary"]["details"][0]["actual"] = "N/A"  # actual 无 string 生产者（评审 M-3）
    assert validate(schema, bad)


# ---------- 防漂移钉子（schema ↔ 实现/文档） ----------


def test_metric_enum_matches_metrics_implementation():
    schema = load("params_snapshot")
    enum = schema["properties"]["assertions"]["items"]["properties"]["metric"]["enum"]
    assert set(enum) == set(METRIC_NAMES)  # day24 契约五指标


def test_substate_enum_matches_protocol_section5():
    schema = load("events")
    status_branch = next(b for b in schema["oneOf"] if b["properties"]["type"]["const"] == "STATUS")
    assert status_branch["properties"]["payload"]["properties"]["substate"]["enum"] == SUBSTATES


def test_params_sample_matches_default_params():
    """样例与场景默认参数同源（day26 定稿值的活样本）。"""
    default = json.loads((SCHEMA_DIR.parents[2] / "lab" / "scenarios" / "normal_delivery"
                          / "params.default.json").read_text(encoding="utf-8"))
    sample = load_sample("params.sample.json")
    for key in ("message_count", "publish_rate", "idle_timeout_s", "assertions"):
        assert sample[key] == default[key]


def test_result_schema_pinned_to_counter_metrics_roundtrip():
    """round-trip 防漂移钉子（day28 评审 I-2）：counter/metrics/evaluate 的真实输出
    直接合成 result 文档过 schema——实现改键名/schema 漂移，任一即红。"""
    from agent.reconcile.counter import count_stages
    from agent.reconcile.metrics import compute_metrics, evaluate_assertions

    events = [
        {"message_id": "m-1", "event": "SEND", "ts": "2026-11-19T10:00:00+00:00"},
        {"message_id": "m-1", "event": "CONFIRM", "ts": "2026-11-19T10:00:01+00:00"},
        {"message_id": "m-1", "event": "DELIVERED", "ts": "2026-11-19T10:00:02+00:00"},
        {"message_id": "m-1", "event": "PROCESSED", "ts": "2026-11-19T10:00:03+00:00"},
        {"message_id": "m-1", "event": "ACKED", "ts": "2026-11-19T10:00:04+00:00"},
    ]
    producer_summary = {"sent": 1, "confirmed": 1}
    consumer_summary = {"acked": 1}
    counts = count_stages(events, producer_summary=producer_summary,
                          consumer_summary=consumer_summary)
    metrics = compute_metrics(counts, events=events)
    assertions = [{"metric": "processed_rate", "op": ">=", "value": 1.0}]
    verdict = evaluate_assertions(metrics, assertions)

    result_doc = {
        "attempt": 1, "status": "COMPLETED",
        "summary": {
            **{k: counts[k] for k in ("sent", "confirmed", "delivered",
                                       "processed_unique", "acked", "dup_business")},
            **metrics,
            "verdict": verdict["verdict"],
            "details": verdict["details"],
        },
        "reconciliation": {k: counts[k] for k in (
            "dup_business_ids", "sent_not_confirmed", "confirmed_not_delivered",
            "delivered_not_processed", "processed_not_acked", "total_events",
        )},
        "artifacts": ["e/records.jsonl.zip"],
    }
    errors = validate(load("result"), result_doc)
    assert errors == [], errors
