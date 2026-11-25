"""协议三 schema 校验测试（第 28 天验收=样例 JSON 通过 schema 校验；负样本真实红）。

schema 是协议 v1.0.1 委托的细化权威；本测试同时钉三处防漂移：
metric 枚举=metrics.METRIC_NAMES、params 样例值=params.default.json、substate 九值集。
"""

import json
from pathlib import Path

from agent.reconcile.metrics import METRIC_NAMES
from jsonschema import Draft202012Validator, FormatChecker

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


# ---------- 协议 v1.1：failure（失败原因）/ faults（故障时间线） ----------


def valid_fault() -> dict:
    """v1.1 最小合法故障记录：consumer_kill 已注入、已解除、验证通过。八键显式给出。"""
    return {
        "strategy": "consumer_kill", "target": "brp_42_consumer",
        "injected_at": "2026-11-24T10:00:10.500+00:00",
        "recovered_at": "2026-11-24T10:00:25.100+00:00",
        "verified_at": "2026-11-24T10:00:33.000+00:00",
        "status": "recovered", "recovery_status": "ok", "error": None,
    }


def test_result_v11_samples_pass():
    schema = load("result")
    for name in ("result_failed.sample.json", "result_fault.sample.json"):
        assert validate(schema, load_sample(name)) == [], name


def test_result_v11_failure_rules():
    schema = load("result")
    good = load_sample("result_failed.sample.json")
    assert validate(schema, good) == []  # 基线：不带 failure 的 schema 会让下面的负例"因别的原因被拒"而假绿

    def patched(**patch) -> dict:
        doc = json.loads(json.dumps(good))
        doc["failure"].update(patch)
        return doc

    assert validate(schema, patched(reason="crashed"))            # reason 枚举外
    assert validate(schema, patched(message=""))                  # 空说明
    assert validate(schema, patched(message="x" * 1025))          # 超长（上限 1024）
    assert validate(schema, patched(stack="…"))                   # 未知键

    doc = json.loads(json.dumps(good))
    del doc["failure"]["message"]                                 # 缺必填
    assert validate(schema, doc)

    doc = json.loads(json.dumps(good))
    doc["status"] = "COMPLETED"                                    # failure 仅限 FAILED
    assert validate(schema, doc)

    # FAILED 不带 failure 仍然合法：不因为缺原因把整份结果拒掉（Go 侧 exit_reason 兜底 error）
    doc = json.loads(json.dumps(good))
    del doc["failure"]
    assert validate(schema, doc) == []


def test_result_v11_fault_rules():
    schema = load("result")
    good = load_sample("result_fault.sample.json")
    assert validate(schema, good) == []  # 基线（同上：负例须在"正例能过"的前提下才有判别力）

    def with_fault(**patch) -> dict:
        doc = json.loads(json.dumps(good))
        doc["faults"] = [{**valid_fault(), **patch}]
        return doc

    # 三种终态各自的自洽形态都合法
    still_injected = {"status": "injected", "recovered_at": None,
                      "verified_at": None, "recovery_status": None}
    assert validate(schema, with_fault(**still_injected)) == []
    inject_failed = {"status": "failed", "injected_at": None, "recovered_at": None, "verified_at": None,
                     "recovery_status": None, "error": "docker: no such container"}  # 注入未生效：什么都没发生过
    assert validate(schema, with_fault(**inject_failed)) == []
    forced = {"recovery_status": "forced", "verified_at": None,
              "error": "recover timed out; forced cleanup"}
    assert validate(schema, with_fault(**forced)) == []
    no_faults = json.loads(json.dumps(good))
    no_faults["faults"] = []                                       # 空数组=无故障
    assert validate(schema, no_faults) == []

    # 词汇
    assert validate(schema, with_fault(status="planned"))          # status 枚举外
    assert validate(schema, with_fault(recovery_status="maybe"))   # recovery_status 枚举外
    # status 与时间戳须自洽
    assert validate(schema, with_fault(recovered_at=None))         # recovered 须有解除时刻
    assert validate(schema, with_fault(status="injected"))         # injected 须 recovered_at 为 null
    assert validate(schema, with_fault(status="failed"))           # failed 须 injected_at 为 null
    # 规则补全（2026-11-24 评审 I-2：下列自相矛盾的形态曾全部放行）
    assert validate(schema, with_fault(status="failed", injected_at=None, recovery_status=None))    # failed 但有解除时刻
    assert validate(schema, with_fault(status="failed", injected_at=None, recovered_at=None,
                                        verified_at=None))                                          # failed 却 ok
    assert validate(schema, with_fault(status="injected", recovered_at=None, verified_at=None))    # 仍在生效却 ok
    assert validate(schema, with_fault(status="injected", recovered_at=None, recovery_status=None))  # 仍在生效却有验证时刻
    assert validate(schema, with_fault(verified_at=None))          # ok 须有验证通过时刻
    over_limit = json.loads(json.dumps(good))
    over_limit["faults"] = [valid_fault()] * 33                    # 条数上限 32
    assert validate(schema, over_limit)
    # 合法形态：仍在生效且解除失败（恢复步骤发生过但没解除掉）
    stuck = with_fault(status="injected", recovered_at=None, verified_at=None, recovery_status="recovery_failed")
    assert validate(schema, stuck) == []
    # 结构
    assert validate(schema, with_fault(injected_at="2026-11-24 10:00:10"))  # format date-time
    assert validate(schema, with_fault(injected_at=1763978410))    # 类型错
    assert validate(schema, with_fault(target=""))                 # minLength
    assert validate(schema, with_fault(extra=1))                   # 未知键
    doc = with_fault()
    del doc["faults"][0]["target"]                                 # 缺必填
    assert validate(schema, doc)
    doc = with_fault()
    del doc["faults"][0]["recovery_status"]                        # 不适用也须显式 null
    assert validate(schema, doc)
    doc = json.loads(json.dumps(good))
    doc["faults"] = valid_fault()                                  # 非数组
    assert validate(schema, doc)


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


def test_params_json_composition_roundtrip():
    """day29 评审 I-3/Rec 2：claim envelope → Agent 落盘合成式（协议 §2.3 v1.0.4⑤）
    → params.json 过 schema——钉 task_id 字符串口径与合成产物形态。"""
    claim_response = {
        "task_id": "42",  # v1.0.5：DB bigint 的十进制字符串
        "execution_id": "0f0e0d0c-baad-1dea-dead-beef00000042",
        "attempt": 1,
        "scenario_key": "normal_delivery",
        "params_snapshot": {
            "message_count": 10000, "publish_rate": 500, "idle_timeout_s": 30,
            "assertions": [{"metric": "processed_rate", "op": ">=", "value": 1.0}],
        },
    }
    params_json = {
        **claim_response["params_snapshot"],
        "task_id": claim_response["task_id"],
        "execution_id": claim_response["execution_id"],
    }
    assert validate(load("params_snapshot"), params_json) == []


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
