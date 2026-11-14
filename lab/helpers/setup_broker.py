"""RabbitMQ 标准配置脚本（M1 第 16 天，实施规划决策 9）。

对空 broker 一次配齐实验环境先决拓扑并自检：
- durable 队列 + DLX 死信队列（拓扑声明，同参数重跑幂等）；
- publisher confirm + delivery_mode=2 探针（confirm 通道发布 → get/ack 往返）；
- 死信路径自检（nack(requeue=False) → 观察消息出现在死信队列并消费）。

pika 延迟导入（main 内）：单测用 FakeChannel 复用本模块时无需安装 pika。
用法：uv run --with 'pika>=1.3,<2' python lab/helpers/setup_broker.py --host <broker> [--port 5672]
"""

import argparse
import logging
import sys
import time

# 拓扑契约（第 19/20/21 天 producer/consumer/conftest 引用，勿改名）：
# 业务队列走默认交换机（rk=队列名）；死信经 brp.dlx direct 交换机路由到 brp.q.dlx。
BUSINESS_QUEUE = "brp.q.business"
DLX_EXCHANGE = "brp.dlx"
DEAD_LETTER_QUEUE = "brp.q.dlx"
DEAD_LETTER_ROUTING_KEY = "dead"

PROBE_BODY = b"brp-setup-probe"
PROBE_MESSAGE_ID = "brp-setup-probe"
DLX_WAIT_ROUNDS = 20
DLX_WAIT_INTERVAL_S = 0.25

log = logging.getLogger("brp-setup")


def setup_topology(channel) -> None:
    """声明先决拓扑；同参数重复声明是 AMQP no-op（幂等），参数不一致由 broker 406 拒绝。"""
    channel.exchange_declare(exchange=DLX_EXCHANGE, exchange_type="direct", durable=True)
    channel.queue_declare(queue=DEAD_LETTER_QUEUE, durable=True)
    channel.queue_bind(
        queue=DEAD_LETTER_QUEUE, exchange=DLX_EXCHANGE, routing_key=DEAD_LETTER_ROUTING_KEY
    )
    channel.queue_declare(
        queue=BUSINESS_QUEUE,
        durable=True,
        arguments={
            "x-dead-letter-exchange": DLX_EXCHANGE,
            "x-dead-letter-routing-key": DEAD_LETTER_ROUTING_KEY,
        },
    )


def _drain_queues(channel) -> int:
    """清空两队列的既往残留（上次运行中途失败可能留下旧探针），返回清掉条数。

    残留不清会使本轮探针错位，verify 永久误报 not-empty（评审 I-1）；
    残留计数同时是"环境被上次异常运行污染"的信号。
    """
    drained = 0
    for queue in (BUSINESS_QUEUE, DEAD_LETTER_QUEUE):
        while True:
            method, _, body = channel.basic_get(queue)
            if body is None:
                break
            channel.basic_ack(method.delivery_tag)
            drained += 1
    if drained:
        log.warning("drained %d residue message(s) before verify", drained)
    return drained


def verify_roundtrip(channel, make_props) -> None:
    """confirm + 持久化投递 + 死信路径自检；结束不留残余消息（探针全部消费）。"""
    _drain_queues(channel)
    channel.confirm_delivery()
    props = make_props(delivery_mode=2, message_id=PROBE_MESSAGE_ID)

    def publish_probe():
        channel.basic_publish(
            exchange="", routing_key=BUSINESS_QUEUE, body=PROBE_BODY, properties=props
        )

    # ① confirm 通道发布持久化探针 → 收到并 ack
    publish_probe()
    method, _, body = channel.basic_get(BUSINESS_QUEUE)
    if body is None:
        raise RuntimeError("probe not routed: basic_get empty after publish")
    channel.basic_ack(method.delivery_tag)

    # ② 死信路径：nack(requeue=False) → 消息应出现在死信队列 → 消费 ack
    publish_probe()
    method, _, body = channel.basic_get(BUSINESS_QUEUE)
    if body is None:
        raise RuntimeError("probe not routed: basic_get empty before nack")
    channel.basic_nack(method.delivery_tag, requeue=False)
    for _ in range(DLX_WAIT_ROUNDS):
        method, _, body = channel.basic_get(DEAD_LETTER_QUEUE)
        if body is not None:
            break
        time.sleep(DLX_WAIT_INTERVAL_S)
    else:
        raise RuntimeError(
            f"dead-letter not observed in {DEAD_LETTER_QUEUE} within "
            f"{DLX_WAIT_ROUNDS * DLX_WAIT_INTERVAL_S:.1f}s"
        )
    channel.basic_ack(method.delivery_tag)

    # ③ 终态：两队列必须为空（配齐且自检后无残余）
    for queue in (BUSINESS_QUEUE, DEAD_LETTER_QUEUE):
        _, _, body = channel.basic_get(queue)
        if body is not None:
            raise RuntimeError(f"queue {queue} not empty after verify")


def connect_with_retry(factory, attempts: int = 10, delay_s: float = 3.0):
    """调用 factory 建连接，失败重试；attempts 次后抛 RuntimeError（信息含尝试次数）。"""
    last = None
    for i in range(1, attempts + 1):
        try:
            return factory()
        except Exception as exc:  # 连接类异常统一重试（pika AMQPConnectionError / OSError）
            last = exc
            if i < attempts:
                log.info("connect failed (attempt %d/%d): %s", i, attempts, exc)
                time.sleep(delay_s)
    raise RuntimeError(f"connect failed after {attempts} attempts: {last}") from last


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="RabbitMQ experiment-env standard setup (idempotent, run-safe)"
    )
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=5672)
    parser.add_argument("--user", default="guest")
    parser.add_argument("--password", default="guest")
    parser.add_argument("--vhost", default="/")
    parser.add_argument("--connect-retries", type=int, default=10)
    parser.add_argument("--retry-delay-s", type=float, default=3.0)
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
    logging.getLogger("pika").setLevel(logging.WARNING)  # pika 自身 INFO 噪声关闭，只留本脚本日志

    import pika  # 延迟导入：单测/复用方无需安装 pika

    def factory():
        return pika.BlockingConnection(
            pika.ConnectionParameters(
                host=args.host,
                port=args.port,
                virtual_host=args.vhost,
                credentials=pika.PlainCredentials(args.user, args.password),
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
        channel = conn.channel()
        setup_topology(channel)
        log.info("exchange %s declared (direct, durable)", DLX_EXCHANGE)
        log.info("queue %s declared + bound (rk=%s)", DEAD_LETTER_QUEUE, DEAD_LETTER_ROUTING_KEY)
        log.info("queue %s declared (durable, DLX=%s)", BUSINESS_QUEUE, DLX_EXCHANGE)
        verify_roundtrip(channel, make_props=lambda **kw: pika.BasicProperties(**kw))
        log.info("verify: confirm publish + get/ack OK (delivery_mode=2)")
        log.info("verify: dead-letter path OK (nack -> %s consumed)", DEAD_LETTER_QUEUE)
        log.info("verify: queues empty, no residue")
        return 0
    except Exception as exc:
        log.error("setup/verify failed: %s: %s", type(exc).__name__, exc)
        return 2
    finally:
        try:
            conn.close()
        except Exception:
            pass


if __name__ == "__main__":
    sys.exit(main())
