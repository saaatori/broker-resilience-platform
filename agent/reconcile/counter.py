"""五阶段对账计数器（M1 第 23 天，实施规划 §5）：JSONL 事件 → 阶段计数与差集。

吸收自场景侧 count_stages（day22 评审路径：吸收不重写），口径：
- **六计数全 unique(message_id)**；dup_business 仅由 PROCESSED 重复产生（day20 口径
  ——DELIVERED/ACKED 的重投重复是诚实记录，不计 dup）；DUP_BUSINESS 标记行不参与计数。
- **权威面/证据面分离**：producer_summary 可覆盖 sent/confirmed（每消息至多一次，
  语义匹配）；consumer_summary 仅覆盖 acked（同上）——**delivered/processed 是
  尝试口径（重投再加一）与 unique 字段语义不符，不覆盖**（day23 评审 I-2 裁决）。
  四个阶段差集恒按 JSONL unique 集合差（证据面，可能因 recorder 丢行偏小）。
- **时间维度不在本函数职责**（day23 评审 I-4 契约）：恢复时间类指标须由
  metrics 另读 events（ts）与故障时间线，勿指望本 dict。
- exclude_prefix 空串=不排除（显式语义）；畸形行（缺字段）点名下标 fail-fast。

输出为 dict（summary JSON 形状，决策 6：入 PG 的 reconciliation 字段）。
依赖仅 lab.helpers.events（词汇单一事实源）；零三方依赖（进 M2 镜像——
M2 打包须 COPY lab/helpers/events.py）。
"""

from __future__ import annotations

from collections import Counter

from lab.helpers.events import (
    EVENT_ACKED,
    EVENT_CONFIRM,
    EVENT_DELIVERED,
    EVENT_PROCESSED,
    EVENT_SEND,
    PROBE_PREFIX,
)


def count_stages(
    events: list[dict],
    *,
    producer_summary: dict | None = None,
    consumer_summary: dict | None = None,
    exclude_prefix: str = PROBE_PREFIX,
) -> dict:
    """五阶段计数 + 差集证据。events 为 recorder JSONL 行（{message_id, event, ts}）。"""
    for index, event in enumerate(events):
        if "message_id" not in event or "event" not in event:
            raise ValueError(f"malformed event at index {index}: {event!r:.80}")

    if exclude_prefix:  # 空串=不排除（显式关闭探针过滤）
        real = [e for e in events if not e["message_id"].startswith(exclude_prefix)]
    else:
        real = list(events)

    ids_by_event: dict[str, set[str]] = {}
    processed_counts: Counter = Counter()
    for event in real:  # 单遍扫描（day23 评审 I-3：O(U×N) 双循环在万条级要 10s+）
        ids_by_event.setdefault(event["event"], set()).add(event["message_id"])
        if event["event"] == EVENT_PROCESSED:
            processed_counts[event["message_id"]] += 1

    send_ids = ids_by_event.get(EVENT_SEND, set())
    confirm_ids = ids_by_event.get(EVENT_CONFIRM, set())
    delivered_ids = ids_by_event.get(EVENT_DELIVERED, set())
    acked_ids = ids_by_event.get(EVENT_ACKED, set())

    processed_lines = sum(processed_counts.values())
    dup_business = processed_lines - len(processed_counts)
    dup_ids = sorted(mid for mid, count in processed_counts.items() if count > 1)

    # 权威面：仅语义匹配的键可被 CLI summary 覆盖（每消息至多计一次的计数）
    sent = producer_summary.get("sent", len(send_ids)) if producer_summary else len(send_ids)
    confirmed = (
        producer_summary.get("confirmed", len(confirm_ids))
        if producer_summary
        else len(confirm_ids)
    )
    acked = consumer_summary.get("acked", len(acked_ids)) if consumer_summary else len(acked_ids)

    return {
        "sent": sent,
        "confirmed": confirmed,
        "delivered": len(delivered_ids),
        "processed_unique": len(processed_counts),
        "acked": acked,
        "dup_business": dup_business,
        "dup_business_ids": dup_ids,
        # 证据面：按 JSONL unique 集合差（summary 覆盖不影响差集）
        "sent_not_confirmed": sorted(send_ids - confirm_ids),
        "confirmed_not_delivered": sorted(confirm_ids - delivered_ids),
        "delivered_not_processed": sorted(delivered_ids - set(processed_counts)),
        "processed_not_acked": sorted(set(processed_counts) - acked_ids),
        "total_events": len(events),  # 原始行数（含探针）透明披露
    }
