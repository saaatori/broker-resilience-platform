"""测试消费者（M1 第 20 天，实施规划决策 5/9、§5）：手动 ACK + POST recorder。

day20 三口径：
- ① 任一事件 POST 失败 → basic_nack(requeue=True)——MQ 重投是唯一重试机制，
  重投引起的重复处理真实记 DUP_BUSINESS（无内联重试，语义永远诚实）；
- ② 每条消息三事件 DELIVERED→PROCESSED→ACKED（词汇契约，day23 counter 消费），
  全部 POST 成功后才 basic_ack——ack 了的消息必有三事件在 JSONL；
- ④ 终止双通道：max_messages 达成（cancel）或 idle 超时（队列排空后静默）。

消费连接经 Toxiproxy down 代理（§7，默认 :5673）；prefetch=1。
pika 延迟导入（main 内）；poster 复用 producer.make_poster。

重投契约（day20 评审 M-4，day23 counter 须按此口径）：口径①下重投会在 JSONL 重复记录
消费侧事件——recorder 只对 PROCESSED 去重，DELIVERED/ACKED 每（重）投递各记一行；
下游计数一律按 unique(message_id, event) 口径，DUP_BUSINESS 仅由 PROCESSED 重复产生。

用法：python consumer.py --host <proxy> [--port 5673] --recorder-url <url> [--max-messages 0]
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from dataclasses import dataclass
from datetime import datetime, timezone

from lab.helpers.producer import make_poster
from lab.helpers.setup_broker import BUSINESS_QUEUE, connect_with_retry

# 事件词汇契约（第 22/23 天场景与 counter 共用，勿改名）：
EVENT_DELIVERED = "DELIVERED"
EVENT_PROCESSED = "PROCESSED"
EVENT_ACKED = "ACKED"

log = logging.getLogger("brp-consumer")


@dataclass
class ConsumerSummary:
    delivered: int = 0
    processed: int = 0
    acked: int = 0
    requeued: int = 0
    stopped: str = "idle"  # "max" | "idle"


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def run_consumer(channel, post, max_messages: int, idle_timeout_s: float) -> ConsumerSummary:
    """消费 BUSINESS_QUEUE：每消息三事件 POST 成功后手动 ACK；POST 失败 requeue。

    channel 须为 pika BlockingChannel；max_messages=0 表示不限（靠 idle 超时退出）。
    """
    channel.basic_qos(prefetch_count=1)
    summary = ConsumerSummary()
    for method, properties, body in channel.consume(
        BUSINESS_QUEUE, inactivity_timeout=idle_timeout_s
    ):
        if method is None:  # idle 超时：队列排空后静默退出
            summary.stopped = "idle"
            break
        mid = getattr(properties, "message_id", None) or json.loads(body)["message_id"]

        failed_at = None
        for index, event in enumerate((EVENT_DELIVERED, EVENT_PROCESSED, EVENT_ACKED)):
            if not post({"message_id": mid, "event": event, "ts": _now()}):
                failed_at = event
                break
            if index == 0:
                summary.delivered += 1
            elif index == 1:
                summary.processed += 1
        if failed_at is not None:  # 口径①：requeue 重投，不内联重试
            channel.basic_nack(method.delivery_tag, requeue=True)
            summary.requeued += 1
            log.warning("requeued %s (post failed at %s)", mid, failed_at)
            continue

        channel.basic_ack(method.delivery_tag)  # 手动 ACK：三事件已全部落 recorder
        summary.acked += 1
        if max_messages and summary.acked >= max_messages:
            summary.stopped = "max"
            channel.cancel()
            break
    return summary


def _non_negative_int(value: str) -> int:
    number = int(value)
    if number < 0:
        raise argparse.ArgumentTypeError(f"must be >= 0, got {value}")
    return number


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        prog="consumer", description="BRP test consumer (manual ack + three-event recording)"
    )
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=5673, help="down 代理端口（§7 消费路径）")
    parser.add_argument("--user", default="guest")
    parser.add_argument("--password", default="guest")
    parser.add_argument("--vhost", default="/")
    parser.add_argument(
        "--max-messages", type=_non_negative_int, default=0, help="0=不限（靠 idle 超时退出）"
    )
    parser.add_argument("--idle-timeout-s", type=float, default=30.0)
    parser.add_argument("--recorder-url", required=True)
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
                # 须 ≥ --idle-timeout-s（静默排空期不是 socket 故障）；黑洞防护同 producer
                socket_timeout=max(60, args.idle_timeout_s + 30),
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
        summary = run_consumer(
            conn.channel(),
            post=make_poster(args.recorder_url),
            max_messages=args.max_messages,
            idle_timeout_s=args.idle_timeout_s,
        )
        log.info(
            "summary: delivered=%d processed=%d acked=%d requeued=%d stopped=%s",
            summary.delivered, summary.processed, summary.acked, summary.requeued,
            summary.stopped,
        )
        reached_target = args.max_messages == 0 or summary.acked == args.max_messages
        return 0 if reached_target else 2
    except Exception as exc:  # noqa: BLE001 —— CLI 边界统一转退出码 2（与前作同口径）
        log.error("consumer run failed: %s: %s", type(exc).__name__, exc)
        return 2
    finally:
        try:
            conn.close()
        except Exception:
            pass


if __name__ == "__main__":
    sys.exit(main())
