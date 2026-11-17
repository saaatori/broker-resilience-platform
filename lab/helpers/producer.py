"""测试生产者（M1 第 19 天，实施规划决策 5/9、§5 对账数据流）。

- message_id=UUID4 双写（决策 5 原文）：AMQP properties.message_id 与 payload JSON
  同一值——单源生成、两处落笔，禁两次生成；
- confirm 通道逐条阻塞确认（决策 9），Nack/Unroutable 记失败不中断，通道断开熔断；
- 控速发送：monotonic 时间槽（target=start+i/rate 睡到槽点），防朴素 sleep 累积漂移；
- SEND/CONFIRM 事件：逐条 INFO 日志（验收 grep 目标）；--recorder-url 可选时按
  day18 行格式 {message_id, event, ts} POST 给 recorder（五阶段计数数据源），
  POST 失败容忍计数，不阻发布主线。

pika 延迟导入（main 内）；运行经 uv run --with 'pika>=1.3,<2'。
用法：python producer.py --host <proxy|broker> [--port 5672] [--count 1000] [--rate 100]
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import time
import urllib.request
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone

from lab.helpers.events import EVENT_CONFIRM, EVENT_SEND
from lab.helpers.setup_broker import BUSINESS_QUEUE, connect_with_retry

log = logging.getLogger("brp-producer")

RECORDER_POSTER_MISS_LIMIT = 10  # 连续 miss 达此数即熔断 POST（黑洞场景防拖垮时间线）


@dataclass
class ProducerSummary:
    sent: int
    confirmed: int
    failed: int
    recorder_misses: int = 0


def make_poster(recorder_url: str):
    """recorder POST 工厂（producer/consumer 共用）：短超时（1s）防阻塞主线，失败返 False。"""

    def post(event: dict) -> bool:
        try:
            req = urllib.request.Request(
                recorder_url,
                data=json.dumps(event, ensure_ascii=False).encode("utf-8"),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=1.0) as resp:
                return resp.status == 200
        except Exception:
            return False

    return post


def run_producer(
    channel,
    count: int,
    rate: float,
    make_props,
    recorder_url: str | None = None,
    post=None,
    sleep_fn=time.sleep,
    now_fn=time.monotonic,
) -> ProducerSummary:
    """控速发布 count 条消息并逐条确认；返回计数汇总。

    channel 须为 pika BlockingChannel（已建连）；make_props 注入 properties 工厂
    （单测用 dict 替身）；rate=0 表示不限速。
    """
    channel.confirm_delivery()
    poster = post if post is not None else (make_poster(recorder_url) if recorder_url else None)
    summary = ProducerSummary(sent=0, confirmed=0, failed=0)
    consecutive_misses = 0
    start = now_fn()

    def post_event(mid: str, event: str) -> None:
        nonlocal poster, consecutive_misses
        if poster is None:
            return
        if poster(
            {"message_id": mid, "event": event, "ts": datetime.now(timezone.utc).isoformat()}
        ):
            consecutive_misses = 0
            return
        summary.recorder_misses += 1
        consecutive_misses += 1
        if consecutive_misses >= RECORDER_POSTER_MISS_LIMIT:
            poster = None  # 熔断：recorder 黑洞（pause/丢包）时不再逐条等超时
            log.warning(
                "recorder poster disabled after %d consecutive misses "
                "(%d misses total, publishing continues)",
                consecutive_misses, summary.recorder_misses,
            )

    for i in range(count):
        if rate and i:  # 时间槽控速：锚定起始，睡到槽点（publish 耗时自动被吸收）
            delay = (start + i / rate) - now_fn()
            if delay > 0:
                sleep_fn(delay)

        mid = str(uuid.uuid4())  # 单源生成
        body = json.dumps({"message_id": mid, "seq": i}).encode("utf-8")
        log.info("[%s] seq=%d id=%s", EVENT_SEND, i, mid)
        post_event(mid, EVENT_SEND)
        summary.sent += 1
        try:
            channel.basic_publish(
                exchange="",  # 默认交换机直发（setup_broker 拓扑契约）
                routing_key=BUSINESS_QUEUE,
                body=body,
                properties=make_props(delivery_mode=2, message_id=mid),
                mandatory=True,  # 口径③：不可路由=UnroutableError=计失败，confirmed=路由+持久化 ack
            )
            log.info("[%s] seq=%d id=%s", EVENT_CONFIRM, i, mid)
            post_event(mid, EVENT_CONFIRM)
            summary.confirmed += 1
        except Exception as exc:  # Nack/Unroutable/通道异常：记失败继续（千条鲁棒）
            summary.failed += 1
            log.error("[FAILED] seq=%d id=%s: %s", i, mid, exc)
            if not getattr(channel, "is_open", True):
                log.critical("channel closed, aborting remaining %d message(s)", count - i - 1)
                break
    return summary


def _non_negative_int(value: str) -> int:
    number = int(value)
    if number < 0:
        raise argparse.ArgumentTypeError(f"must be >= 0, got {value}")
    return number


def _non_negative_float(value: str) -> float:
    number = float(value)
    if number < 0:
        raise argparse.ArgumentTypeError(f"must be >= 0, got {value}")
    return number


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        prog="producer", description="BRP test producer (uuid4 double-write + confirm + pacing)"
    )
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=5672)
    parser.add_argument("--user", default="guest")
    parser.add_argument("--password", default="guest")
    parser.add_argument("--vhost", default="/")
    parser.add_argument("--count", type=_non_negative_int, default=1000)
    parser.add_argument(
        "--rate", type=_non_negative_float, default=100.0, help="msg/s，0=不限速"
    )
    parser.add_argument("--recorder-url", default=None, help="可选：SEND/CONFIRM 事件 POST 地址")
    parser.add_argument("--connect-retries", type=int, default=10)
    parser.add_argument("--retry-delay-s", type=float, default=3.0)
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
    logging.getLogger("pika").setLevel(logging.WARNING)

    import pika  # 延迟导入：单测/复用方无需安装 pika

    def factory():
        return pika.BlockingConnection(
            pika.ConnectionParameters(
                host=args.host,
                port=args.port,
                virtual_host=args.vhost,
                credentials=pika.PlainCredentials(args.user, args.password),
                socket_timeout=30,  # day20 评审记档：DROP 黑洞下 basic_publish 不无限阻塞
            )
        )

    try:
        conn = connect_with_retry(
            factory, attempts=args.connect_retries, delay_s=args.retry_delay_s
        )
    except RuntimeError as exc:
        log.error("%s", exc)
        return 2

    try:
        summary = run_producer(
            conn.channel(),
            count=args.count,
            rate=args.rate,
            make_props=lambda **kw: pika.BasicProperties(**kw),
            recorder_url=args.recorder_url,
        )
        log.info(
            "summary: sent=%d confirmed=%d failed=%d recorder_misses=%d",
            summary.sent, summary.confirmed, summary.failed, summary.recorder_misses,
        )
        return 0 if summary.failed == 0 and summary.confirmed == args.count else 2
    except Exception as exc:  # noqa: BLE001 —— CLI 边界统一转退出码 2（与前作同口径）
        log.error("producer run failed: %s: %s", type(exc).__name__, exc)
        return 2
    finally:
        try:
            conn.close()
        except Exception:
            pass


if __name__ == "__main__":
    sys.exit(main())
