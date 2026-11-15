"""consumer 单测：纯逻辑核心（Fake channel.consume 生成器 + 注入 post）+ main 级。

语义出处（day20 三口径）：①任一事件 POST 失败 → nack(requeue=True)，MQ 重投是
唯一重试机制，重复处理真实记 DUP_BUSINESS；②每消息三事件 DELIVERED→PROCESSED→ACKED
全部成功后才 ack；④终止双通道（max_messages / idle 超时）。
"""

import json
import sys
import types

import pytest

from lab.helpers.consumer import (
    EVENT_ACKED,
    EVENT_DELIVERED,
    EVENT_PROCESSED,
    run_consumer,
)
from lab.helpers.setup_broker import BUSINESS_QUEUE


class FakeMethod:
    def __init__(self, tag):
        self.delivery_tag = tag


class FakeProps:
    def __init__(self, message_id):
        self.message_id = message_id


class FakeConsumerChannel:
    """consume() 产出既定消息后 yield (None, None, None)（模拟 inactivity 超时）。"""

    def __init__(self, messages):
        self.messages = list(messages)  # [(tag, mid, body)]
        self.acked, self.nacked, self.cancelled = [], [], False
        self.qos = None

    def basic_qos(self, prefetch_count):
        self.qos = prefetch_count

    def consume(self, queue, inactivity_timeout=None):
        assert queue == BUSINESS_QUEUE
        for tag, mid, body in self.messages:
            yield FakeMethod(tag), FakeProps(mid), body
        yield None, None, None

    def basic_ack(self, delivery_tag):
        self.acked.append(delivery_tag)

    def basic_nack(self, delivery_tag, requeue=None):
        self.nacked.append((delivery_tag, requeue))

    def cancel(self):
        self.cancelled = True


def make_messages(n):
    return [
        (i + 1, f"m-{i + 1}", json.dumps({"message_id": f"m-{i + 1}", "seq": i}).encode())
        for i in range(n)
    ]


def ok_post(events):
    def post(event):
        events.append(event)
        return True

    return post


def test_consumes_posts_three_events_then_acks():
    ch = FakeConsumerChannel(make_messages(3))
    events = []

    summary = run_consumer(ch, ok_post(events), max_messages=3, idle_timeout_s=5)

    assert (summary.delivered, summary.processed, summary.acked, summary.requeued) == (3, 3, 3, 0)
    assert summary.stopped == "max"
    for i in range(1, 4):
        seq = [e["event"] for e in events if e["message_id"] == f"m-{i}"]
        assert seq == [EVENT_DELIVERED, EVENT_PROCESSED, EVENT_ACKED]  # 三事件有序
    assert ch.acked == [1, 2, 3]  # ack 在三事件成功之后
    assert ch.nacked == []
    assert ch.cancelled is True
    assert ch.qos == 1  # prefetch=1
    assert all(isinstance(e["ts"], str) and e["ts"] for e in events)


def test_post_failure_on_delivered_nacks_requeue():
    ch = FakeConsumerChannel(make_messages(3))
    events = []

    def flaky_post(event):
        if event["message_id"] == "m-2" and event["event"] == EVENT_DELIVERED:
            return False
        events.append(event)
        return True

    summary = run_consumer(ch, flaky_post, max_messages=2, idle_timeout_s=5)

    assert summary.acked == 2  # m-1、m-3
    assert summary.requeued == 1
    assert ch.nacked == [(2, True)]  # requeue=True，MQ 重投是唯一重试（口径①）
    assert ch.acked == [1, 3]


def test_post_failure_on_processed_and_acked_also_requeue():
    ch = FakeConsumerChannel(make_messages(2))

    def flaky_post(event):
        if event["message_id"] == "m-1" and event["event"] == EVENT_PROCESSED:
            return False
        if event["message_id"] == "m-2" and event["event"] == EVENT_ACKED:
            return False
        return True

    summary = run_consumer(ch, flaky_post, max_messages=2, idle_timeout_s=5)

    assert summary.acked == 0  # 两单都在成功前被 requeue
    assert summary.requeued == 2
    assert sorted(ch.nacked) == [(1, True), (2, True)]  # 口径①全路径


def test_stops_at_max_messages():
    ch = FakeConsumerChannel(make_messages(3))
    events = []

    summary = run_consumer(ch, ok_post(events), max_messages=2, idle_timeout_s=5)

    assert summary.acked == 2
    assert summary.stopped == "max"
    assert ch.cancelled is True
    assert len(events) == 6  # 2 消息 × 3 事件，第 3 条未处理


def test_idle_timeout_stops():
    ch = FakeConsumerChannel([])
    events = []

    summary = run_consumer(ch, ok_post(events), max_messages=0, idle_timeout_s=1)

    assert (summary.acked, summary.stopped) == (0, "idle")
    assert events == []


def test_message_id_falls_back_to_payload():
    ch = FakeConsumerChannel([(1, None, json.dumps({"message_id": "m-x", "seq": 0}).encode())])
    events = []

    run_consumer(ch, ok_post(events), max_messages=1, idle_timeout_s=5)

    assert {e["message_id"] for e in events} == {"m-x"}  # properties 缺失 → payload 兜底


# ---------- main（假 pika 注入） ----------


def _fake_pika_module(channel):
    class FakeConnection:
        def channel(self):
            return channel

        def close(self):
            pass

    return types.SimpleNamespace(
        BlockingConnection=lambda params: FakeConnection(),
        ConnectionParameters=lambda **kw: None,
        PlainCredentials=lambda user, password: None,
        BasicProperties=lambda **kw: dict(kw),
    )


def test_main_happy_path_returns_0(monkeypatch, caplog, tmp_path):
    """全链集成钉子：main(假 pika) → make_poster(真 urllib) → day18 recorder(真 HTTP)。"""
    import threading

    import lab.helpers.consumer as consumer_mod
    from lab.helpers.recorder import Recorder, make_server

    rec = Recorder(tmp_path / "r.jsonl")
    server = make_server(rec, host="127.0.0.1", port=0)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        monkeypatch.setitem(
            sys.modules, "pika", _fake_pika_module(FakeConsumerChannel(make_messages(1)))
        )

        with caplog.at_level("INFO"):
            rc = consumer_mod.main(
                [
                    "--max-messages", "1",
                    "--recorder-url", f"http://127.0.0.1:{server.server_address[1]}/records",
                    "--connect-retries", "1", "--retry-delay-s", "0",
                ]
            )

        assert rc == 0
        assert "acked=1" in caplog.text
        lines = [
            json.loads(line)
            for line in (tmp_path / "r.jsonl").read_text(encoding="utf-8").splitlines()
        ]
        assert [line["event"] for line in lines] == [
            EVENT_DELIVERED, EVENT_PROCESSED, EVENT_ACKED,
        ]  # 全链三事件落 JSONL
    finally:
        server.shutdown()
        server.server_close()


def test_main_exit_2_when_connect_fails(monkeypatch, caplog):
    import lab.helpers.consumer as consumer_mod

    def connect_fail(params):
        raise OSError("connection refused")

    fake_pika = _fake_pika_module(FakeConsumerChannel([]))
    fake_pika.BlockingConnection = connect_fail
    monkeypatch.setitem(sys.modules, "pika", fake_pika)

    with caplog.at_level("INFO"):
        rc = consumer_mod.main(
            [
                "--max-messages", "1", "--recorder-url", "http://recorder/records",
                "--connect-retries", "1", "--retry-delay-s", "0",
            ]
        )

    assert rc == 2
    assert "connect failed after 1 attempts" in caplog.text


def test_max_messages_rejected_when_negative():
    """负值 --max-messages 拒之门外（day20 评审 M-3，与 producer --count 同口径）。"""
    import lab.helpers.consumer as consumer_mod

    with pytest.raises(SystemExit) as exc_info:
        consumer_mod.main(["--max-messages", "-1", "--recorder-url", "http://r/records"])
    assert exc_info.value.code == 2
