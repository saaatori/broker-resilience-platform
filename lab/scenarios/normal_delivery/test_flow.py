"""场景①正常投递（M1 第 22 天，实施规划 §5/§6）：发 N 条 → 消费 → 阶段计数断言。

编排：网内 docker exec producer/consumer 容器（conftest run_helper）；
断言链：CLI summary → JSONL 五阶段计数（排除 fixture-probe- 前缀，day21 契约）
→ 队列被动声明清零。纯函数（parse_summary/count_stages）宿主单测覆盖；
count_stages 是 day23 counter.py 正式化前的场景轻量版。
"""

from __future__ import annotations

import json
import re
import urllib.request
import uuid
from collections import Counter, defaultdict

import pytest

from lab.conftest import (
    PROBE_PREFIX,
    PROXY_AMQP_PORT,
    PROXY_AMQP_PORT_DOWN,
    amqp_parameters,
)
from lab.helpers.consumer import (
    EVENT_ACKED,
    EVENT_DELIVERED,
    EVENT_PROCESSED,
)
from lab.helpers.producer import EVENT_CONFIRM, EVENT_SEND
from lab.helpers.setup_broker import BUSINESS_QUEUE

ALL_FIVE_EVENTS = (EVENT_SEND, EVENT_CONFIRM, EVENT_DELIVERED, EVENT_PROCESSED, EVENT_ACKED)

SUMMARY_MARKER = "summary:"


def test_normal_delivery_end_to_end(params, task_id, recorder_url, run_helper, read_jsonl):
    """发 N → 消费 N → 五阶段断言。步骤序固定（Review Focus 3：探针先行、producer 先行）。"""
    n = params["message_count"]
    rate = params.get("publish_rate", 0)

    # ⓪ 残留护栏（评审 M-4）：上次运行未 down 的 JSONL 残留会让计数翻倍且难归因
    residue = [e for e in read_jsonl() if not e["message_id"].startswith(PROBE_PREFIX)]
    if residue:
        pytest.fail(
            f"检测到上次运行残留 {len(residue)} 行（task_id={task_id} 的卷未清理）——"
            f"先 lab_factory down 或换 task_id 再跑"
        )

    # ① recorder 存活探针（day20 I-1②：healthcheck 不覆盖运行中崩溃；行入 JSONL 由前缀契约排除）
    probe = {"message_id": f"{PROBE_PREFIX}{uuid.uuid4()}", "event": EVENT_DELIVERED, "ts": "t"}
    req = urllib.request.Request(
        recorder_url, data=json.dumps(probe).encode(), headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        assert resp.status == 200

    # ② producer 先行（无 idle 竞态），经 up 代理发布
    code, output = run_helper(
        "producer",
        "--host", f"brp_{task_id}_proxy", "--port", str(PROXY_AMQP_PORT),
        "--count", str(n), "--rate", str(rate),
        "--recorder-url", recorder_url,
    )
    assert code == 0, output[-2000:]
    producer = parse_summary(output, required=("sent", "confirmed", "failed", "recorder_misses"))
    assert producer["sent"] == n and producer["confirmed"] == n and producer["failed"] == 0
    assert producer["recorder_misses"] == 0  # 评审 M-5：区分"消息丢失"与"记录器丢事件"

    # ③ consumer 主通道 max_messages=N（day20 评审 rec），经 down 代理
    code, output = run_helper(
        "consumer",
        "--host", f"brp_{task_id}_proxy", "--port", str(PROXY_AMQP_PORT_DOWN),
        "--max-messages", str(n), "--idle-timeout-s", "30",
        "--recorder-url", recorder_url,
    )
    assert code == 0, output[-2000:]
    consumer = parse_summary(
        output, required=("delivered", "processed", "acked", "requeued", "stopped")
    )
    assert consumer["acked"] == n and consumer["requeued"] == 0 and consumer["stopped"] == "max"

    # ④ JSONL 五阶段计数（排除探针前缀；每 id 恰五事件；无重复业务副作用）
    result = count_stages(read_jsonl())
    assert result["counts"] == {event: n for event in ALL_FIVE_EVENTS}
    assert result["total_ids"] == n
    assert result["incomplete_ids"] == []

    # ⑤ 队列清零（评审 I-1：内联短开短关——编排期间不持有连接，防 pika 心跳断连）
    import pika

    conn = pika.BlockingConnection(amqp_parameters(f"brp_{task_id}_proxy"))
    try:
        method = conn.channel().queue_declare(BUSINESS_QUEUE, passive=True)
        assert method.method.message_count == 0
    finally:
        conn.close()

def parse_summary(output: str, required: tuple[str, ...] = ("sent", "confirmed", "failed")) -> dict:
    """从 CLI 输出解析最后一行 "summary: k=v k=v ..."（int 可转则转，否则原字符串）。

    缺 summary 行或缺任一 required 字段 → ValueError 点名（fail-fast，不让断言在
    千行 INFO 噪声里静默漏检）。
    """
    summary_lines = [line for line in output.splitlines() if SUMMARY_MARKER in line]
    if not summary_lines:
        raise ValueError("summary line not found in helper output")
    pairs = dict(re.findall(r"(\w+)=(\S+)", summary_lines[-1]))
    missing = [key for key in required if key not in pairs]
    if missing:
        raise ValueError(f"summary missing field(s): {', '.join(missing)}")
    return {k: int(v) if v.lstrip("-").isdigit() else v for k, v in pairs.items()}


def count_stages(events: list[dict], exclude_prefix: str = PROBE_PREFIX) -> dict:
    """五阶段计数（场景轻量版）：排除探针前缀行，点名事件不完整的 message_id。

    day21 契约：fixture-probe-* 行是烟雾探针写入共享卷的，计数一律排除。
    **口径注意（day23 counter 吸收时）**：counts 为逐行口径（重复行计入——本场景
    借此承担 dup 检测）；day23 counter 的阶段计数须另取 unique(message_id, event)
    口径，DUP_BUSINESS 单列，勿直接搬走 counts。
    """
    real = [e for e in events if not e["message_id"].startswith(exclude_prefix)]
    counts = Counter(e["event"] for e in real)
    by_id: dict[str, set[str]] = defaultdict(set)
    for event in real:
        by_id[event["message_id"]].add(event["event"])
    expected = set(ALL_FIVE_EVENTS)
    incomplete = sorted(mid for mid, seen in by_id.items() if seen != expected)
    return {"counts": dict(counts), "total_ids": len(by_id), "incomplete_ids": incomplete}
