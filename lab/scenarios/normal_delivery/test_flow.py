"""场景①正常投递（M1 第 22 天，实施规划 §5/§6）：发 N 条 → 消费 → 阶段计数断言。

编排：网内 docker exec producer/consumer 容器（conftest run_helper）；
断言链：CLI summary → JSONL 五阶段计数（agent.reconcile.counter，day23 收口）
→ 指标计算与 verdict 断言（agent.reconcile.metrics，day25 全链）
→ 队列被动声明清零。parse_summary/resolve_assertions 纯函数宿主单测覆盖。
"""

from __future__ import annotations

import json
import re
import urllib.request
import uuid
from pathlib import Path

import pytest

from agent.reconcile.counter import count_stages  # day23 收口：吸收本模块原实现
from agent.reconcile.metrics import compute_metrics, evaluate_assertions
from lab.conftest import (
    PROXY_AMQP_PORT,
    PROXY_AMQP_PORT_DOWN,
    amqp_parameters,
)
from lab.helpers.events import EVENT_DELIVERED, PROBE_PREFIX
from lab.helpers.setup_broker import BUSINESS_QUEUE

SUMMARY_MARKER = "summary:"

# 场景默认断言集（day25 万条验收口径）：params["assertions"] 可覆盖（schema=day24 契约）
DEFAULT_SCENARIO_ASSERTIONS = (
    {"metric": "processed_rate", "op": ">=", "value": 1.0},
    {"metric": "dup_rate", "op": "<=", "value": 0.0},
    {"metric": "loss_rate", "op": "<=", "value": 0.0},
)


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

    # ③ consumer 主通道 max_messages=N（day20 评审 rec），经 down 代理；
    # idle 兜底超时参数化（day26 定稿默认 30，params.idle_timeout_s 可覆盖）
    idle_timeout_s = params.get("idle_timeout_s", 30)
    code, output = run_helper(
        "consumer",
        "--host", f"brp_{task_id}_proxy", "--port", str(PROXY_AMQP_PORT_DOWN),
        "--max-messages", str(n), "--idle-timeout-s", str(idle_timeout_s),
        "--recorder-url", recorder_url,
    )
    assert code == 0, output[-2000:]
    consumer = parse_summary(
        output, required=("delivered", "processed", "acked", "requeued", "stopped")
    )
    assert consumer["acked"] == n and consumer["requeued"] == 0 and consumer["stopped"] == "max"

    # ④ JSONL 五阶段计数（day23 counter）：双面断言——
    # 权威面（喂 summary）：六计数与 CLI 一致；证据面（不喂 summary）：钉"记录链路忠实"
    # （day23 评审 I-1：仅喂 summary 抓不住 recorder 静默丢整消息行/多余非 PROCESSED 行）
    events = read_jsonl()
    result = count_stages(events, producer_summary=producer, consumer_summary=consumer)
    assert (
        result["sent"],
        result["confirmed"],
        result["delivered"],
        result["processed_unique"],
        result["acked"],
    ) == (n, n, n, n, n)
    assert result["dup_business"] == 0 and result["dup_business_ids"] == []

    evidence = count_stages(events)  # 证据面：纯 JSONL 计数
    assert (
        evidence["sent"],
        evidence["confirmed"],
        evidence["delivered"],
        evidence["processed_unique"],
        evidence["acked"],
    ) == (n, n, n, n, n)
    assert evidence["dup_business"] == 0
    for diff_key in (
        "sent_not_confirmed",
        "confirmed_not_delivered",
        "delivered_not_processed",
        "processed_not_acked",
    ):
        assert result[diff_key] == [] and evidence[diff_key] == []
    # 总行数锚定：恰 5n + 探针行（探针数按前缀自数——网内同目录跑 test_fixtures 会多 1 行）
    probe_count = sum(1 for e in events if e["message_id"].startswith(PROBE_PREFIX))
    assert evidence["total_events"] == 5 * n + probe_count

    # ④.5 指标与断言（day25 万条验收全链消费：metrics 吃权威面 counts + 全量 events）
    metrics = compute_metrics(result, events=events)
    verdict = evaluate_assertions(metrics, resolve_assertions(params))

    # ④.6 结果落盘（day40 runner 契约）：results.json 写在子进程 CWD（=工作目录），
    # 键集 = result schema 的 summary/reconciliation（与 day28 round-trip 钉子同源）。
    # 写在 verdict 断言之前：断言失败（FAIL）时证据仍在——runner 对 rc≠0 也读回 results。
    Path("results.json").write_text(
        json.dumps({
            "summary": {
                **{k: result[k] for k in (
                    "sent", "confirmed", "delivered", "processed_unique", "acked", "dup_business",
                )},
                **metrics,
                "verdict": verdict["verdict"],
                "details": verdict["details"],
            },
            "reconciliation": {k: result[k] for k in (
                "dup_business_ids", "sent_not_confirmed", "confirmed_not_delivered",
                "delivered_not_processed", "processed_not_acked", "total_events",
            )},
        }, ensure_ascii=False),
        encoding="utf-8",
    )

    assert verdict["verdict"] == "PASS", f"verdict details: {verdict['details']}"

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


def resolve_assertions(params: dict) -> list[dict]:
    """断言集解析：缺省/None → 场景默认集；**显式空列表 → 保留**（day25 评审 I-1）。

    空集是合法显式输入（"无断言"），与 evaluate_assertions 的 vacuous-PASS 契约一致；
    用 `or` 回落会吞掉空列表造成配置快照与执行事实背离。day31 schema 冻结前此语义不变。
    """
    assertions = params.get("assertions")
    return list(DEFAULT_SCENARIO_ASSERTIONS) if assertions is None else assertions
