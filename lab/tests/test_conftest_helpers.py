"""conftest 纯函数单测（宿主可跑，零 pika/零网络）。

fixture 本体是薄壳（组装纯函数），语义全部钉在纯函数层；
amqp_connection 的真连接路径由 scenarios/normal_delivery 烟雾测试在网内验收覆盖。
"""

import json

import pytest

from lab.conftest import load_params, recorder_url_for


def test_load_params_reads_object(tmp_path):
    path = tmp_path / "params.json"
    path.write_text(json.dumps({"task_id": "t1", "message_count": 5}), encoding="utf-8")

    assert load_params(path) == {"task_id": "t1", "message_count": 5}


def test_load_params_rejects_missing_file(tmp_path):
    with pytest.raises(RuntimeError, match="not found"):
        load_params(tmp_path / "nope.json")


def test_load_params_rejects_invalid_json(tmp_path):
    path = tmp_path / "params.json"
    path.write_text("{broken", encoding="utf-8")

    with pytest.raises(RuntimeError, match="invalid JSON"):
        load_params(path)


def test_load_params_rejects_non_object(tmp_path):
    path = tmp_path / "params.json"
    path.write_text("[1, 2]", encoding="utf-8")

    with pytest.raises(RuntimeError, match="object"):
        load_params(path)


def test_load_params_rejects_unreadable_path(tmp_path):
    """目录等不可读路径也归入统一 RuntimeError 口径（评审 M-2）。"""
    with pytest.raises(RuntimeError, match="not readable"):
        load_params(tmp_path)  # 目录


def test_load_params_rejects_invalid_encoding(tmp_path):
    path = tmp_path / "params.json"
    path.write_bytes(b"\xff\xfe{")  # 非 UTF-8 字节

    with pytest.raises(RuntimeError, match="encoding"):
        load_params(path)


def test_recorder_url_for():
    assert recorder_url_for("t1") == "http://brp_t1_recorder:8080/records"
