"""setup_broker 纯逻辑单测。

FakeChannel 鸭子类型模拟 AMQP broker 最小行为（默认交换机投递、ack/nack、
nack(requeue=False) 触发死信转发），因此单测无需安装 pika——pika 由 main()
延迟导入，仅真实连 broker 时用到（计划 Task 2 Interfaces）。
"""

import logging
import sys
import types

import pytest

from lab.helpers.setup_broker import (
    BUSINESS_QUEUE,
    DEAD_LETTER_QUEUE,
    DLX_EXCHANGE,
    connect_with_retry,
    setup_topology,
    verify_roundtrip,
)


class ChannelClosedFake(Exception):
    """模拟 pika ChannelClosedByBroker（如 406 PRECONDITION_FAILED）。"""


class FakeChannel:
    """记录 channel 调用并模拟 broker 侧最小语义。

    死信路由保真（评审 M-2）：nack(requeue=False) 按源队列 declare 时的
    x-dead-letter-exchange / x-dead-letter-routing-key 参数，经 queue_bind 记录
    查找目标队列；无对应绑定则丢弃（真实 broker 对不可路由死信即丢弃）——
    因此绑定类缺陷（如 rk 写错）在单测中可见，而非被 Fake 无条件转发掩盖。
    """

    def __init__(self, fail_on_queue_declare=None):
        self.calls = []
        self.queues = {BUSINESS_QUEUE: [], DEAD_LETTER_QUEUE: []}
        self.queue_args = {}  # queue -> declare 时的 arguments
        self._pending = {}
        self._next_tag = 1
        self._fail_declare = fail_on_queue_declare  # (queue, exc)

    def _record(self, name, **kw):
        self.calls.append((name, kw))

    def exchange_declare(self, **kw):
        self._record("exchange_declare", **kw)

    def queue_declare(self, **kw):
        queue = kw["queue"]
        if self._fail_declare and queue == self._fail_declare[0]:
            raise self._fail_declare[1]
        self.queues.setdefault(queue, [])
        self.queue_args[queue] = kw.get("arguments") or {}
        self._record("queue_declare", **kw)

    def queue_bind(self, **kw):
        self._record("queue_bind", **kw)

    def confirm_delivery(self):
        self._record("confirm_delivery")

    def basic_publish(self, **kw):
        self._record("basic_publish", **kw)
        self.queues[kw["routing_key"]].append(kw)  # 默认交换机：routing_key 即队列名

    def basic_get(self, queue, auto_ack=False):
        self._record("basic_get", queue=queue)
        if not self.queues[queue]:
            return None, None, None
        item = self.queues[queue].pop(0)
        tag = self._next_tag
        self._next_tag += 1
        self._pending[tag] = (queue, item)
        method = type("Method", (), {"delivery_tag": tag})()
        return method, item["properties"], item["body"]

    def basic_ack(self, delivery_tag):
        self._record("basic_ack", delivery_tag=delivery_tag)
        self._pending.pop(delivery_tag, None)

    def basic_nack(self, delivery_tag, requeue=None):
        self._record("basic_nack", delivery_tag=delivery_tag, requeue=requeue)
        source_queue, item = self._pending.pop(delivery_tag)
        if requeue is False:  # broker 行为：不重回队列 → 按 DLX 参数经绑定路由
            args = self.queue_args.get(source_queue, {})
            dlx = args.get("x-dead-letter-exchange")
            rk = args.get("x-dead-letter-routing-key")
            if dlx is not None:
                for name, kw in self.calls:
                    if name == "queue_bind" and kw["exchange"] == dlx and kw["routing_key"] == rk:
                        self.queues[kw["queue"]].append(item)
                        break
                        # 无匹配绑定 → 丢弃（真实 broker 对不可路由死信同样丢弃）


def calls_of(ch, name):
    return [kw for n, kw in ch.calls if n == name]


def test_topology_declares_exact_topology():
    ch = FakeChannel()
    setup_topology(ch)

    assert calls_of(ch, "exchange_declare") == [
        {"exchange": DLX_EXCHANGE, "exchange_type": "direct", "durable": True}
    ]
    assert calls_of(ch, "queue_declare") == [
        {"queue": DEAD_LETTER_QUEUE, "durable": True},
        {
            "queue": BUSINESS_QUEUE,
            "durable": True,
            "arguments": {
                "x-dead-letter-exchange": DLX_EXCHANGE,
                "x-dead-letter-routing-key": "dead",
            },
        },
    ]
    assert calls_of(ch, "queue_bind") == [
        {"queue": DEAD_LETTER_QUEUE, "exchange": DLX_EXCHANGE, "routing_key": "dead"}
    ]


def test_setup_is_idempotent_under_same_args():
    ch = FakeChannel()
    setup_topology(ch)
    first = list(ch.calls)

    setup_topology(ch)  # 同参数重跑：不得抛错、调用序列一致

    assert ch.calls[len(first):] == first


def test_mismatched_existing_queue_fails_fast():
    ch = FakeChannel(
        fail_on_queue_declare=(BUSINESS_QUEUE, ChannelClosedFake("406 PRECONDITION_FAILED"))
    )

    with pytest.raises(ChannelClosedFake):  # fail-fast，不静默跳过
        setup_topology(ch)


def test_verify_roundtrip_publishes_persistent_and_deadletters():
    ch = FakeChannel()
    setup_topology(ch)  # 死信路由依赖 declare/bind 记录（Fake 路由保真）

    verify_roundtrip(ch, make_props=lambda **kw: dict(kw))

    publishes = calls_of(ch, "basic_publish")
    assert len(publishes) == 2
    for kw in publishes:
        assert kw["exchange"] == ""
        assert kw["routing_key"] == BUSINESS_QUEUE
        assert kw["body"] == b"brp-setup-probe"
        assert kw["properties"] == {"delivery_mode": 2, "message_id": "brp-setup-probe"}
    nacks = calls_of(ch, "basic_nack")
    assert len(nacks) == 1 and nacks[0]["requeue"] is False
    # 结束无残余：两队列空
    assert ch.queues[BUSINESS_QUEUE] == []
    assert ch.queues[DEAD_LETTER_QUEUE] == []
    # 次序：探针 ack → 死信 nack → 死信 ack
    names = [n for n, _ in ch.calls]
    ack_positions = [i for i, n in enumerate(names) if n == "basic_ack"]
    assert len(ack_positions) == 2
    assert ack_positions[0] < names.index("basic_nack") < ack_positions[1]


def test_verify_roundtrip_drains_stale_residue():
    """上次运行中途失败留下的探针残留不得使本次自检永久误报（评审 I-1）。"""
    ch = FakeChannel()
    setup_topology(ch)  # 死信路由依赖 declare/bind 记录（Fake 路由保真）
    stale = {
        "exchange": "",
        "routing_key": BUSINESS_QUEUE,
        "body": b"brp-setup-probe",
        "properties": {"delivery_mode": 2, "message_id": "brp-setup-probe"},
    }
    ch.queues[BUSINESS_QUEUE].append(dict(stale))
    ch.queues[DEAD_LETTER_QUEUE].append(dict(stale))

    verify_roundtrip(ch, make_props=lambda **kw: dict(kw))  # 旧残留被 drain，不抛错

    assert ch.queues[BUSINESS_QUEUE] == []
    assert ch.queues[DEAD_LETTER_QUEUE] == []


def test_main_returns_2_and_logs_on_declare_rejection(monkeypatch, caplog):
    """406 拒绝 → main 捕获 → exit 2 + 明示日志（Review Focus 1 端到端钉子，评审 I-2）。

    现有行为的回归保护（pin）：写此测试时代码路径已存在，无 RED 阶段。
    """
    import lab.helpers.setup_broker as sb

    fake_channel = FakeChannel(
        fail_on_queue_declare=(BUSINESS_QUEUE, ChannelClosedFake("406 PRECONDITION_FAILED"))
    )

    class FakeConnection:
        def channel(self):
            return fake_channel

        def close(self):
            pass

    fake_pika = types.SimpleNamespace(
        BlockingConnection=lambda params: FakeConnection(),
        ConnectionParameters=lambda **kw: None,
        PlainCredentials=lambda user, password: None,
        BasicProperties=lambda **kw: None,
    )
    monkeypatch.setitem(sys.modules, "pika", fake_pika)  # 利用延迟导入注入假模块

    with caplog.at_level(logging.INFO):
        rc = sb.main(["--connect-retries", "1", "--retry-delay-s", "0"])

    assert rc == 2
    assert "setup/verify failed" in caplog.text
    assert "406" in caplog.text


def test_verify_roundtrip_dead_letter_timeout_reports_queue(monkeypatch):
    """死信超时分支（for/else）：无绑定路由时消息被丢弃，报错须点名队列（评审 M-3/M-2）。"""
    import lab.helpers.setup_broker as sb

    monkeypatch.setattr(sb, "DLX_WAIT_ROUNDS", 2)
    monkeypatch.setattr(sb, "DLX_WAIT_INTERVAL_S", 0)

    ch = FakeChannel()  # 故意不跑 setup_topology：无 DLX 参数/绑定记录 → 死信被丢弃

    with pytest.raises(RuntimeError, match="dead-letter not observed in brp.q.dlx"):
        verify_roundtrip(ch, make_props=lambda **kw: dict(kw))


def test_connect_with_retry_gives_up():
    attempts = []

    def failing_factory():
        attempts.append(1)
        raise OSError("connection refused")

    with pytest.raises(RuntimeError, match=r"after 3 attempts"):
        connect_with_retry(failing_factory, attempts=3, delay_s=0)
    assert len(attempts) == 3
