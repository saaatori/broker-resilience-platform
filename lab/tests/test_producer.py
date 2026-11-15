"""producer 单测：纯逻辑核心（Fake channel，无需 pika）+ main 级（假 pika 注入）。

语义出处：实施规划决策 5（UUID4 双写：properties.message_id == payload.message_id）、
决策 9（delivery_mode=2 + confirm）、§5（SEND/CONFIRM 事件、五阶段 sent/confirmed）。
"""

import json
import sys
import types
import uuid

import pytest

from lab.helpers.producer import run_producer
from lab.helpers.setup_broker import BUSINESS_QUEUE

DICT_PROPS = lambda **kw: dict(kw)  # noqa: E731 -- 测试替身


class FakeChannel:
    def __init__(self, fail_on=None, close_on_exception=False):
        self.published = []
        self.is_open = True
        self._fail_on = fail_on or set()  # 失败的调用序号（第几次 publish，从 0 起）
        self._close_on_exception = close_on_exception
        self._calls = 0

    def confirm_delivery(self):
        pass

    def basic_publish(self, exchange, routing_key, body, properties=None, mandatory=False):
        call_no = self._calls
        self._calls += 1
        if call_no in self._fail_on:
            if self._close_on_exception:
                self.is_open = False
            raise RuntimeError("nack simulated")
        self.published.append(
            {"exchange": exchange, "routing_key": routing_key, "body": body,
             "properties": properties}
        )


def test_publishes_unique_uuid4_double_written():
    ch = FakeChannel()

    run_producer(ch, count=5, rate=0, make_props=DICT_PROPS)

    assert len(ch.published) == 5
    ids = set()
    for item in ch.published:
        assert item["exchange"] == ""
        assert item["routing_key"] == BUSINESS_QUEUE
        props_mid = item["properties"]["message_id"]
        payload_mid = json.loads(item["body"])["message_id"]
        assert props_mid == payload_mid  # 双写：单源同值（决策 5）
        uuid.UUID(props_mid)  # 合法 UUID4 格式
        assert uuid.UUID(props_mid).version == 4  # 钉死版本位（契约非实现巧合）
        ids.add(props_mid)
        assert item["properties"]["delivery_mode"] == 2
    assert len(ids) == 5  # 全局唯一


def test_summary_all_confirmed(caplog):
    ch = FakeChannel()

    with caplog.at_level("INFO"):
        summary = run_producer(ch, count=5, rate=0, make_props=DICT_PROPS)

    assert (summary.sent, summary.confirmed, summary.failed) == (5, 5, 0)
    import re

    send_ids = set(re.findall(r"\[SEND\] seq=\d+ id=(\S+)", caplog.text))
    confirm_ids = set(re.findall(r"\[CONFIRM\] seq=\d+ id=(\S+)", caplog.text))
    assert send_ids == confirm_ids == {item["properties"]["message_id"] for item in ch.published}


def test_publish_failure_recorded_and_continues():
    ch = FakeChannel(fail_on={2})  # 第 3 次（序号 2）失败

    summary = run_producer(ch, count=5, rate=0, make_props=DICT_PROPS)

    assert (summary.sent, summary.confirmed, summary.failed) == (5, 4, 1)
    assert len(ch.published) == 4  # 失败的那条没入队，但循环走完 5 次


def test_stops_when_channel_closed():
    ch = FakeChannel(fail_on={0}, close_on_exception=True)  # 首发即失败且通道关闭

    summary = run_producer(ch, count=100, rate=0, make_props=DICT_PROPS)

    assert summary.failed >= 1
    assert summary.sent < 100  # 熔断：不在死通道上空转


def test_rate_pacing_uses_anchored_time_slots():
    """锚定 vs 朴素要可区分（评审 I-1）：publish 消耗 fake 时间 0.03s，
    锚定实现产出 [0.07,0.07,0.07]（槽点差吸收 publish 耗时）；
    朴素 sleep(1/rate) 会产出 [0.1,0.1,0.1]——本断言即区分二者。"""
    now = {"t": 100.0}
    sleeps = []

    def fake_now():
        return now["t"]

    def fake_sleep(delay):
        sleeps.append(round(delay, 6))
        now["t"] += delay

    class SlowPublishChannel(FakeChannel):
        def basic_publish(self, *args, **kw):
            now["t"] += 0.03  # 每条 publish 耗 0.03s（漂移来源）
            return super().basic_publish(*args, **kw)

    ch = SlowPublishChannel()
    run_producer(ch, count=4, rate=10, make_props=DICT_PROPS, sleep_fn=fake_sleep, now_fn=fake_now)

    assert sleeps == [0.07, 0.07, 0.07]  # 锚定：0.1 槽点 - 0.03 publish 耗时

    sleeps.clear()
    run_producer(ch, count=3, rate=0, make_props=DICT_PROPS, sleep_fn=fake_sleep, now_fn=fake_now)
    assert sleeps == []  # rate=0 不限速


def test_recorder_events_posted():
    ch = FakeChannel()
    posted = []

    def recording_post(event):
        posted.append(event)
        return True

    run_producer(
        ch, count=2, rate=0, make_props=DICT_PROPS,
        recorder_url="http://recorder/records", post=recording_post,
    )

    events = [(p["message_id"], p["event"]) for p in posted]
    published_mids = {item["properties"]["message_id"] for item in ch.published}
    assert len(events) == 4  # 2 消息 × (SEND+CONFIRM)
    assert all(mid in published_mids for mid, _ in events)
    # 每条消息 SEND 先于其 CONFIRM
    for mid in published_mids:
        assert [e for m, e in events if m == mid] == ["SEND", "CONFIRM"]
    assert all(isinstance(p["ts"], str) and p["ts"] for p in posted)  # day18 行格式


def test_recorder_post_failure_tolerated():
    ch = FakeChannel()
    misses = {"n": 0}

    def failing_post(event):
        misses["n"] += 1
        return False

    summary = run_producer(
        ch, count=3, rate=0, make_props=DICT_PROPS,
        recorder_url="http://recorder/records", post=failing_post,
    )

    assert (summary.sent, summary.confirmed, summary.failed) == (3, 3, 0)  # 发布不受影响
    assert summary.recorder_misses == 6  # 3×(SEND+CONFIRM) 全 miss
    assert misses["n"] == 6


# ---------- main（假 pika 注入，day17 I-2 同款模式） ----------


def _fake_pika_module(fake_channel):
    class FakeConnection:
        def channel(self):
            return fake_channel

        def close(self):
            pass

    return types.SimpleNamespace(
        BlockingConnection=lambda params: FakeConnection(),
        ConnectionParameters=lambda **kw: None,
        PlainCredentials=lambda user, password: None,
        BasicProperties=lambda **kw: dict(kw),
    )


def test_main_happy_path_returns_0(monkeypatch, caplog):
    import lab.helpers.producer as producer_mod

    monkeypatch.setitem(sys.modules, "pika", _fake_pika_module(FakeChannel()))

    with caplog.at_level("INFO"):
        rc = producer_mod.main(
            ["--count", "3", "--rate", "0", "--connect-retries", "1", "--retry-delay-s", "0"]
        )

    assert rc == 0
    assert "sent=3 confirmed=3 failed=0" in caplog.text  # summary 行


def test_main_exit_2_when_connect_fails(monkeypatch, caplog):
    import lab.helpers.producer as producer_mod

    def connect_fail(params):
        raise OSError("connection refused")

    fake_pika = _fake_pika_module(FakeChannel())
    fake_pika.BlockingConnection = connect_fail
    monkeypatch.setitem(sys.modules, "pika", fake_pika)

    with caplog.at_level("INFO"):
        rc = producer_mod.main(
            ["--count", "1", "--connect-retries", "1", "--retry-delay-s", "0"]
        )

    assert rc == 2
    assert "connect failed after 1 attempts" in caplog.text


def test_main_exit_2_when_channel_setup_fails(monkeypatch, caplog):
    """运行段异常（如 confirm_delivery 抛错）→ log.error + exit 2，不裸奔（评审 I-2）。"""
    import lab.helpers.producer as producer_mod

    class DeadChannel(FakeChannel):
        def confirm_delivery(self):
            raise RuntimeError("channel died during setup")

    monkeypatch.setitem(sys.modules, "pika", _fake_pika_module(DeadChannel()))

    with caplog.at_level("INFO"):
        rc = producer_mod.main(
            ["--count", "1", "--connect-retries", "1", "--retry-delay-s", "0"]
        )

    assert rc == 2
    assert "channel died during setup" in caplog.text


def test_recorder_poster_circuit_breaker_after_consecutive_misses():
    """recorder 黑洞熔断（评审 M-3）：连续 10 次 miss 后停止 POST，发布不受影响。"""
    ch = FakeChannel()
    calls = {"n": 0}

    def blackhole_post(event):
        calls["n"] += 1
        return False

    summary = run_producer(
        ch, count=15, rate=0, make_props=DICT_PROPS,
        recorder_url="http://recorder/records", post=blackhole_post,
    )

    assert (summary.sent, summary.confirmed, summary.failed) == (15, 15, 0)
    assert summary.recorder_misses == 10  # 第 10 次连续 miss 后熔断
    assert calls["n"] == 10  # 之后不再尝试


def test_count_and_rate_rejected_when_negative():
    import lab.helpers.producer as producer_mod

    with pytest.raises(SystemExit) as exc_info:
        producer_mod.main(["--count", "-1"])
    assert exc_info.value.code == 2  # argparse error 统一 exit 2

    with pytest.raises(SystemExit) as exc_info:
        producer_mod.main(["--rate", "-5"])
    assert exc_info.value.code == 2


def test_make_poster_against_real_recorder_server(tmp_path):
    """_make_poster 真实 urllib 路径 × day18 recorder 互通（评审 M-6）。"""
    import threading

    from lab.helpers.producer import _make_poster
    from lab.helpers.recorder import Recorder, make_server

    rec = Recorder(tmp_path / "r.jsonl")
    server = make_server(rec, host="127.0.0.1", port=0)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"
        poster = _make_poster(base + "/records")

        assert poster({"message_id": "m-1", "event": "SEND", "ts": "t1"}) is True
        assert poster({"message_id": "m-1", "event": "CONFIRM", "ts": "t2"}) is True
        lines = [
            json.loads(line)
            for line in (tmp_path / "r.jsonl").read_text(encoding="utf-8").splitlines()
        ]
        assert [line["event"] for line in lines] == ["SEND", "CONFIRM"]

        assert _make_poster(base + "/nope")({"x": 1}) is False  # 404 → False
    finally:
        server.shutdown()
        server.server_close()
