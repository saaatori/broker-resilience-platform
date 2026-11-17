"""五指标计算与断言判定（M1 第 24 天，实施规划 §5/表 22/演示主线）：纯函数。

五指标口径（docstring 即契约，day25 万条验收 / M2 uploader / day54 场景消费）：
- processed_rate = processed_unique / sent（处理率）；
- dup_rate = dup_business / (processed_unique + dup_business)（重复副作用占总处理次数——
  丢消息场景不失真：分母是实际发生的处理，而非发送量）；
- loss_rate = (sent - acked) / sent（端到端丢失率，任意阶段丢失并入；分段细节见 counter 差集）；
- avg_delivery_latency_s：events 中 SEND 与 ACKED 齐全的 id 的（首 ACKED − 首 SEND）均值
  ——day23 I-4 契约：时间维度不在 counter 职责，本函数另读 events；
- recovery_time_s：仅当 fault={injected_at, recovered_at}（绝对 ISO 时间戳）提供时 = 差值秒数。
  **形状鸿沟记档**：params_snapshot.fault 是 {strategy, inject_at_s, duration_s}（相对秒），
  由 harness（场景编排）负责换算为绝对时间戳再传入——勿把快照原样塞进来。

**0 分母 → None（不崩、不撒谎）**；**越界率（<0 或 >1）视为数据污染信号**——
evaluate_assertions 对率类指标做值域检查，越界记 FAIL+reason（不做 clamp）。
events 探针行按 exclude_prefix 过滤（与 counter 契约对称，day24 评审 I-1）。
依赖零三方（进 M2 镜像）；counts 为 agent.reconcile.counter 输出（子集键即可）。
"""

from __future__ import annotations

import operator
from datetime import datetime

from lab.helpers.events import EVENT_ACKED, EVENT_SEND, PROBE_PREFIX

METRIC_NAMES = (
    "processed_rate",
    "dup_rate",
    "loss_rate",
    "avg_delivery_latency_s",
    "recovery_time_s",
)

RATE_METRICS = ("processed_rate", "dup_rate", "loss_rate")  # 值域 [0,1]，越界=污染信号


def _validate_events(events: list[dict]) -> None:
    for index, event in enumerate(events):
        missing = [k for k in ("message_id", "event", "ts") if k not in event]
        if missing:
            raise ValueError(f"malformed event at index {index}: missing {missing}")


def _first_ts_by_id(events: list[dict], event_name: str) -> dict[str, datetime]:
    first: dict[str, datetime] = {}
    for event in events:
        if event["event"] != event_name:
            continue
        mid = event["message_id"]
        try:
            parsed = datetime.fromisoformat(event["ts"])
        except ValueError:
            raise ValueError(f"malformed ts for {event_name} of {mid}: {event['ts']!r}") from None
        if mid not in first or parsed < first[mid]:
            first[mid] = parsed
    return first


def _avg_latency(events: list[dict]) -> float | None:
    sends = _first_ts_by_id(events, EVENT_SEND)
    ackeds = _first_ts_by_id(events, EVENT_ACKED)
    deltas = [
        (ackeds[mid] - sends[mid]).total_seconds()
        for mid in ackeds.keys() & sends.keys()  # 只算两事件齐全的 id（不齐排除）
    ]
    if not deltas:
        return None
    return sum(deltas) / len(deltas)


def _recovery_time(fault: dict) -> float | None:
    if not fault:
        return None
    for key in ("injected_at", "recovered_at"):
        if key not in fault:
            raise ValueError(f"fault missing key: {key}")
    injected = datetime.fromisoformat(fault["injected_at"])
    recovered = datetime.fromisoformat(fault["recovered_at"])
    return (recovered - injected).total_seconds()


def compute_metrics(
    counts: dict,
    *,
    events: list[dict] | None = None,
    fault: dict | None = None,
    exclude_prefix: str = PROBE_PREFIX,
) -> dict:
    """counts=counter 输出（子集键即可）；events/fault 可选（时间类指标数据源）。

    events 为 recorder JSONL 全量行即可——探针行按 exclude_prefix 过滤（空串=关闭）。
    """
    sent = counts.get("sent", 0)
    acked = counts.get("acked", 0)
    processed_unique = counts.get("processed_unique", 0)
    dup_business = counts.get("dup_business", 0)
    processed_total = processed_unique + dup_business

    usable_events = None
    if events:
        _validate_events(events)
        usable_events = (
            [e for e in events if not e["message_id"].startswith(exclude_prefix)]
            if exclude_prefix
            else events
        )

    return {
        "processed_rate": processed_unique / sent if sent else None,
        "dup_rate": dup_business / processed_total if processed_total else None,
        "loss_rate": (sent - acked) / sent if sent else None,
        "avg_delivery_latency_s": _avg_latency(usable_events) if usable_events else None,
        "recovery_time_s": _recovery_time(fault),
    }


_OPS = {
    "==": operator.eq,
    ">=": operator.ge,
    "<=": operator.le,
    ">": operator.gt,
    "<": operator.lt,
}


def evaluate_assertions(metrics: dict, assertions: list[dict]) -> dict:
    """断言判定：schema {metric, op, value}；输出 verdict + 逐条明细。

    - None 指标（0 分母/无数据）→ FAIL + reason（杜绝 vacuous pass）；
    - 率类指标越界（<0 或 >1）→ FAIL + 污染 reason（不做 clamp，不洗白数据问题）；
    - 未知 op/未知 metric/value 非数值/metrics 缺键 → ValueError 点名（fail-fast）；
    - 空 assertions → PASS（vacuous，显式口径：无可失败项）。
    """
    details = []
    for assertion in assertions:
        metric, op, value = assertion["metric"], assertion["op"], assertion["value"]
        if metric not in METRIC_NAMES:
            raise ValueError(f"unknown metric in assertion: {metric}")
        if op not in _OPS:
            raise ValueError(f"unknown op in assertion: {op!r}")
        if not isinstance(value, (int, float)) or isinstance(value, bool):
            raise TypeError(f"assertion value must be a number, got {value!r}")
        if metric not in metrics:
            raise ValueError(f"metrics dict missing key: {metric}")
        actual = metrics[metric]
        if actual is None:
            details.append(
                {
                    "metric": metric,
                    "op": op,
                    "value": value,
                    "actual": None,
                    "passed": False,
                    "reason": "metric undefined (zero denominator or no data)",
                }
            )
            continue
        if metric in RATE_METRICS and not 0.0 <= actual <= 1.0:
            details.append(
                {
                    "metric": metric,
                    "op": op,
                    "value": value,
                    "actual": actual,
                    "passed": False,
                    "reason": "metric out of range [0,1] — data pollution suspected "
                    "(e.g. stale JSONL or mismatched summaries)",
                }
            )
            continue
        details.append(
            {
                "metric": metric,
                "op": op,
                "value": value,
                "actual": actual,
                "passed": _OPS[op](actual, value),
            }
        )
    return {
        "verdict": "PASS" if all(d["passed"] for d in details) else "FAIL",
        "details": details,
    }
