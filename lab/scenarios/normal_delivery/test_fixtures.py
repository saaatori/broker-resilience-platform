"""conftest 三 fixture 接线烟雾测试（M1 第 21 天验收载荷）。

**只在实验网内跑**（见 lab/conftest.py 模块 docstring）：runner 先
lab_factory up + setup_broker，再在网内容器里执行本文件。
day22 的 test_flow.py 将以本文件为起点扩展。

**探针计数契约（day21 评审 I-1）**：本文件向 recorder POST 的探针事件使用
`fixture-probe-` 前缀的 message_id——它会写入实验 JSONL（这是验证 POST 契约
的代价）；day22 场景断言与 day23 counter 一律**排除该前缀的行**再计数。
"""

import json
import socket
import urllib.request
import uuid
from urllib.parse import urlparse

PROBE_PREFIX = "fixture-probe-"


def test_params_and_recorder_url_wired(params, task_id, recorder_url):
    assert isinstance(params, dict)
    assert params.get("task_id", "manual") == task_id
    assert recorder_url == f"http://brp_{task_id}_recorder:8080/records"
    # 环境感知（评审 M-3）：recorder 主机名必须可解析——宿主误跑/配错 task_id 即本测试全红
    socket.getaddrinfo(urlparse(recorder_url).hostname, 8080)


def test_amqp_connection_via_proxy_and_recorder_probe(amqp_connection, recorder_url):
    # AMQP 连接经 toxiproxy up 代理（brp_<task_id>_proxy:5672）
    assert amqp_connection.is_open

    # recorder 探针：POST 一条 DELIVERED 事件，断言响应回执（评审 I-1：行入 JSONL，
    # 计数侧按 fixture-probe- 前缀排除，见模块 docstring 契约）
    probe = {
        "message_id": f"{PROBE_PREFIX}{uuid.uuid4()}",
        "event": "DELIVERED",
        "ts": "2026-11-16T00:00:00+00:00",
    }
    req = urllib.request.Request(
        recorder_url,
        data=json.dumps(probe).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        body = json.loads(resp.read().decode("utf-8"))
        assert body["status"] == "recorded"
        assert body["appended"][0]["message_id"] == probe["message_id"]  # 回执校验
