"""recorder 单测：核心语义（文件级）+ HTTP 层（进程内真服务器，port=0）。

语义出处：实施规划 §5——JSONL 行 {message_id, event, ts}；message_id 重复出现在
PROCESSED → 追加 DUP_BUSINESS 事件；决策 11——事件时间戳以 consumer 上报优先。
"""

import json
import threading
import urllib.error
import urllib.request
from datetime import datetime
from pathlib import Path

from lab.helpers.recorder import DUP_BUSINESS, PROCESSED, Recorder


def read_lines(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line]


def test_first_processed_appends_single_line(tmp_path):
    path = tmp_path / "records.jsonl"
    rec = Recorder(path)

    appended = rec.record("m-001", PROCESSED, ts="2026-11-15T10:00:00+00:00")

    assert appended == [
        {"message_id": "m-001", "event": "PROCESSED", "ts": "2026-11-15T10:00:00+00:00"}
    ]
    assert read_lines(path) == appended  # 已落盘


def test_duplicate_processed_appends_dup_business_line(tmp_path):
    rec = Recorder(tmp_path / "records.jsonl")

    rec.record("m-001", PROCESSED, ts="t1")
    appended = rec.record("m-001", PROCESSED, ts="t2")

    assert [a["event"] for a in appended] == [PROCESSED, DUP_BUSINESS]
    lines = read_lines(rec.path)
    assert len(lines) == 3
    assert lines[2] == {"message_id": "m-001", "event": DUP_BUSINESS, "ts": "t2"}


def test_only_processed_triggers_dup_business(tmp_path):
    rec = Recorder(tmp_path / "records.jsonl")

    for _ in range(3):
        rec.record("m-001", "DELIVERED", ts="t")  # 非 PROCESSED 重复不触发
    assert not any(line["event"] == DUP_BUSINESS for line in read_lines(rec.path))

    rec.record("m-001", PROCESSED, ts="t4")
    rec.record("m-001", PROCESSED, ts="t5")  # PROCESSED 重复触发
    assert any(line["event"] == DUP_BUSINESS for line in read_lines(rec.path))


def test_seen_set_rebuilt_from_existing_jsonl_on_restart(tmp_path):
    path = tmp_path / "records.jsonl"
    Recorder(path).record("m-001", PROCESSED, ts="t1")

    rec = Recorder(path)  # 模拟容器重建、卷保留后的重启

    appended = rec.record("m-001", PROCESSED, ts="t2")
    assert [a["event"] for a in appended] == [PROCESSED, DUP_BUSINESS]  # seen 集已重建


def test_ts_consumer_value_wins_otherwise_server_utc(tmp_path):
    rec = Recorder(tmp_path / "records.jsonl")

    rec.record("m-001", PROCESSED, ts="2026-11-15T10:00:00+00:00")
    rec.record("m-002", PROCESSED)  # 缺 ts → 服务器补 UTC now

    lines = read_lines(rec.path)
    assert lines[0]["ts"] == "2026-11-15T10:00:00+00:00"
    parsed = datetime.fromisoformat(lines[1]["ts"])
    assert parsed.tzinfo is not None  # 带时区，可解析


def test_record_rejects_invalid_fields(tmp_path):
    """字段类型/空值校验在库级 record()（评审 I-1）：行格式契约的唯一执行点。"""
    import pytest

    rec = Recorder(tmp_path / "records.jsonl")

    with pytest.raises(ValueError, match="message_id"):
        rec.record("", PROCESSED)
    with pytest.raises(ValueError, match="message_id"):
        rec.record(123, PROCESSED)
    with pytest.raises(ValueError, match="event"):
        rec.record("m-001", "")
    with pytest.raises(ValueError, match="ts"):
        rec.record("m-001", PROCESSED, ts=12345)  # ts 必须是字符串（ISO 8601）或缺省
    assert not rec.path.exists()  # 全拒之门外：零污染


def test_replay_skips_corrupt_lines(tmp_path):
    path = tmp_path / "records.jsonl"
    path.write_text(
        '{"message_id": "m-001", "event": "PROCESSED", "ts": "t1"}\n'
        "{这条是坏行\n"
        "\n",
        encoding="utf-8",
    )

    rec = Recorder(path)  # 坏行跳过不崩，好行入 seen

    appended = rec.record("m-001", PROCESSED, ts="t2")
    assert [a["event"] for a in appended] == [PROCESSED, DUP_BUSINESS]


# ---------- HTTP 层（进程内真服务器，port=0） ----------


def start_server(tmp_path):
    from lab.helpers.recorder import make_server

    rec = Recorder(tmp_path / "records.jsonl")
    server = make_server(rec, host="127.0.0.1", port=0)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return rec, server


def http_post(url: str, body: bytes) -> tuple[int, dict]:
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8") or "{}")


def test_http_two_duplicate_posts_yield_dup_business(tmp_path):
    """逐日验收的自动化微缩：两次同 id PROCESSED → 第二次响应与文件均含 DUP_BUSINESS。"""
    rec, server = start_server(tmp_path)
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"
        body = json.dumps(
            {"message_id": "m-001", "event": "PROCESSED", "ts": "t1"}
        ).encode("utf-8")

        status1, resp1 = http_post(base + "/records", body)
        status2, resp2 = http_post(base + "/records", body)

        assert (status1, status2) == (200, 200)
        assert [a["event"] for a in resp2["appended"]] == [PROCESSED, DUP_BUSINESS]
        assert any(line["event"] == DUP_BUSINESS for line in read_lines(rec.path))
    finally:
        server.shutdown()
        server.server_close()


def test_invalid_payload_rejected_without_writing(tmp_path):
    """畸形 payload → 400/404，且文件零污染（不写半行）。"""
    rec, server = start_server(tmp_path)
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"

        status_bad_json, _ = http_post(base + "/records", b"{not json")
        status_missing, _ = http_post(
            base + "/records", json.dumps({"event": "PROCESSED"}).encode("utf-8")
        )
        status_404, _ = http_post(base + "/nope", b"{}")
        status_empty_id, _ = http_post(
            base + "/records",
            json.dumps({"message_id": "", "event": "PROCESSED"}).encode("utf-8"),
        )
        status_bad_ts, _ = http_post(
            base + "/records",
            json.dumps({"message_id": "m", "event": "PROCESSED", "ts": 12345}).encode("utf-8"),
        )

        assert (status_bad_json, status_missing, status_404) == (400, 400, 404)
        assert (status_empty_id, status_bad_ts) == (400, 400)
        assert not rec.path.exists()  # 零污染：从未写入（连文件都不该有）
    finally:
        server.shutdown()
        server.server_close()


def test_concurrent_same_id_posts_append_dup_per_repeat(tmp_path):
    """并发同 id POST（评审 Rec-2 钉子）：N 线程 → 8×P + 7×DUP = 15 行。

    §5 语义：每个重复 PROCESSED 各追加一行 DUP_BUSINESS；单锁全原子保证
    "首个 P 无 DUP、其余每次 P+DUP"的因果，防未来重构退化为两段锁。
    """
    rec, server = start_server(tmp_path)
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"
        body = json.dumps({"message_id": "m-race", "event": "PROCESSED", "ts": "t"}).encode()
        results: list = []
        lock = threading.Lock()

        def poster():
            status, resp = http_post(base + "/records", body)
            with lock:
                results.append((status, resp))

        threads = [threading.Thread(target=poster) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        assert all(status == 200 for status, _ in results)
        dup_responses = [
            resp for _, resp in results if any(a["event"] == DUP_BUSINESS for a in resp["appended"])
        ]
        assert len(dup_responses) == 7  # 除首个外每次重复各带一行 DUP
        lines = read_lines(rec.path)
        assert len(lines) == 15
        assert sum(1 for line in lines if line["event"] == DUP_BUSINESS) == 7
        assert sum(1 for line in lines if line["event"] == PROCESSED) == 8
    finally:
        server.shutdown()
        server.server_close()
