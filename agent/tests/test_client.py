"""executor/client.py 单测（第 38 天）：对真实本机 HTTP 服务验证 wire 形状与错误处置。

FakeGo = http.server 线程上的假控制面：按路径挂处理函数、记录收到的请求。
客户端的传输纪律（逐请求超时 / 忽略环境代理 / 不跟随重定向 / 线程间不共享 Session）
只有走真实传输才钉得住，所以不用 mock 库。
"""

import json
import socket
import threading
import time
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
import requests

import executor.client as client_module
from executor.client import (
    ClaimConflict,
    ClaimedTask,
    ControlClient,
    ControlPlaneError,
    HeartbeatResult,
    LeaseLost,
    Unauthorized,
)

REGISTER = "/api/v1/agent/register"
HEARTBEAT = "/api/v1/agent/heartbeat"
CLAIM = "/api/v1/agent/tasks/claim"

# ---------- FakeGo ----------


class FakeGo:
    """假 Go 控制面（本机回环真实 HTTP）：handlers 按路径分发，requests 记录取到的请求。

    处理函数收到 {"path", "headers", "body"}，返回 (status, payload, extra_headers)；
    payload 为 dict/None 时按 JSON 发送，为 bytes 时原样发送。
    """

    def __init__(self):
        self.handlers = {
            REGISTER: lambda req: (200, {"agent_id": "agent-1", "agent_token": "token-1"}, {}),
            HEARTBEAT: lambda req: (200, {"lease_ok": [], "cancel_requested": []}, {}),
            CLAIM: lambda req: (204, None, {}),
        }
        self.requests = []
        self._lock = threading.Lock()
        go = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):  # 静音（否则每个请求打一行到 stderr）
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                req = {
                    "path": self.path,
                    "headers": dict(self.headers),
                    "body": json.loads(raw) if raw else None,
                }
                with go._lock:
                    go.requests.append(req)
                handler = go.handlers.get(self.path)
                if handler is None:
                    self.send_response(404)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                status, payload, extra = handler(req)
                if isinstance(payload, bytes):
                    data = payload
                else:
                    data = b"" if payload is None else json.dumps(payload).encode()
                self.send_response(status)
                for key, value in extra.items():
                    self.send_header(key, value)
                if isinstance(payload, dict):
                    self.send_header("Content-Type", "application/json")
                if status != 204:  # 204 响应不得带 Content-Length
                    self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._server.handle_error = lambda *args: None  # 客户端先超时断开时，慢处理函数写回会报错，属预期
        # poll_interval 调小：shutdown() 要等一轮 poll 才返回，默认 0.5s 会把每个用例的收尾拖慢
        threading.Thread(target=self._server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()

    @property
    def base_url(self):
        return f"http://127.0.0.1:{self._server.server_address[1]}/api/v1"

    def on(self, path, handler):
        self.handlers[path] = handler

    def to(self, path):
        return [r for r in self.requests if r["path"] == path]

    def close(self):
        self._server.shutdown()
        self._server.server_close()


# ---------- 测试辅助 ----------


def token_of(req):
    return req["headers"].get("X-Agent-Token")


def free_port():
    """取一个当前无人监听的端口（随后关闭，保持"不可达"）。"""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def run_in_threads(fn, count):
    """并发跑 fn，返回 (results, errors)。"""
    results, errors = [], []

    def runner():
        try:
            results.append(fn())
        except Exception as e:  # noqa: BLE001 — 线程内的异常要带回主线程断言
            errors.append(e)

    threads = [threading.Thread(target=runner) for _ in range(count)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=10)
    return results, errors


def rotating_register(go):
    """把 register 换成"每次换发新 token"的处理函数；返回收到的注册体列表（即调用序）。"""
    calls = []

    def handler(req):
        calls.append(req["body"])
        return 200, {"agent_id": "agent-1", "agent_token": f"token-{len(calls)}"}, {}

    go.on(REGISTER, handler)
    return calls


@pytest.fixture
def go():
    server = FakeGo()
    yield server
    server.close()


def new_client(go, **handlers):
    """按给定处理函数建客户端并完成首次注册；返回 (client, 清空请求记录之后的状态)。"""
    for path, handler in handlers.items():
        go.on(path, handler)
    client = ControlClient(go.base_url)
    client.register("brp-agent", "0.1.0")
    go.requests.clear()
    return client


# ---------- wire 形状 ----------


def test_register_sends_body_and_authed_requests_carry_token(go):
    client = ControlClient(go.base_url)

    assert client.register("brp-agent", "0.1.0") == "agent-1"

    (sent,) = go.to(REGISTER)
    assert sent["body"] == {"hostname": "brp-agent", "version": "0.1.0", "max_concurrent": 1}
    assert sent["headers"].get("Content-Type") == "application/json"
    assert "X-Agent-Token" not in sent["headers"]

    client.heartbeat([])
    client.claim()
    assert token_of(go.to(HEARTBEAT)[0]) == "token-1"
    assert token_of(go.to(CLAIM)[0]) == "token-1"


def test_heartbeat_sends_running_list_and_parses_dual_arrays(go):
    client = new_client(
        go, **{HEARTBEAT: lambda req: (200, {"lease_ok": ["eid-a"], "cancel_requested": ["eid-b"]}, {})}
    )

    result = client.heartbeat(["eid-a", "eid-b"])

    assert result == HeartbeatResult(lease_ok=["eid-a"], cancel_requested=["eid-b"])
    body = go.to(HEARTBEAT)[0]["body"]
    assert body == {"agent_id": "agent-1", "running_executions": ["eid-a", "eid-b"]}


def test_claim_200_maps_five_keys(go):
    snapshot = {"message_count": 50, "task_id": "12"}
    client = new_client(
        go,
        **{
            CLAIM: lambda req: (
                200,
                {
                    "task_id": "12",
                    "execution_id": "eid-1",
                    "attempt": 1,
                    "scenario_key": "normal_delivery",
                    "params_snapshot": snapshot,
                },
                {},
            )
        },
    )

    assert client.claim() == ClaimedTask(
        task_id="12",
        execution_id="eid-1",
        attempt=1,
        scenario_key="normal_delivery",
        params_snapshot=snapshot,
    )
    assert go.to(CLAIM)[0]["body"] == {"agent_id": "agent-1"}


def test_claim_204_returns_none(go):
    assert new_client(go).claim() is None


# ---------- 错误映射 ----------


@pytest.mark.parametrize(
    ("status", "expected"),
    [
        (409, ClaimConflict),
        (410, LeaseLost),
        (400, ControlPlaneError),
        (404, ControlPlaneError),
        (500, ControlPlaneError),
        (503, ControlPlaneError),
    ],
)
def test_status_mapping(go, status, expected):
    client = new_client(go, **{CLAIM: lambda req: (status, {"error": "x"}, {})})

    with pytest.raises(expected) as excinfo:
        client.claim()

    assert type(excinfo.value) is expected  # 400 等不得被误报成某个子类
    assert excinfo.value.status == status


def test_unreachable_control_plane_raises_without_status():
    # 指向一个无人监听的端口（关 FakeGo 的监听套接字不够：已建立的 keep-alive 连接还活着）
    client = ControlClient(f"http://127.0.0.1:{free_port()}/api/v1")

    with pytest.raises(ControlPlaneError) as excinfo:
        client.register("brp-agent", "0.1.0")

    assert excinfo.value.status is None


def test_response_missing_field_is_a_control_plane_error(go):
    client = new_client(go, **{HEARTBEAT: lambda req: (200, {"lease_ok": []}, {})})

    with pytest.raises(ControlPlaneError) as excinfo:
        client.heartbeat([])

    assert "cancel_requested" in str(excinfo.value)
    assert excinfo.value.status == 200


def test_non_json_200_is_a_control_plane_error(go):
    client = new_client(
        go,
        **{HEARTBEAT: lambda req: (200, b"<html>oops</html>", {"Content-Type": "text/html"})},
    )

    with pytest.raises(ControlPlaneError, match="JSON"):
        client.heartbeat([])


# ---------- 401：单飞重注册 ----------


def test_401_reregisters_with_same_hostname_then_retries_once(go):
    register_calls = rotating_register(go)
    client = new_client(
        go,
        **{
            HEARTBEAT: lambda req: (
                (200, {"lease_ok": [], "cancel_requested": []}, {})
                if token_of(req) == "token-2"
                else (401, {"error": "agent token 无效"}, {})
            )
        },
    )

    result = client.heartbeat(["eid-1"])

    assert result == HeartbeatResult(lease_ok=[], cancel_requested=[])
    assert register_calls == [
        {"hostname": "brp-agent", "version": "0.1.0", "max_concurrent": 1}
    ] * 2  # 首次 + 一次重注册，注册体相同（协议 §2.1 幂等键=hostname）
    beats = go.to(HEARTBEAT)
    assert [token_of(r) for r in beats] == ["token-1", "token-2"]
    assert beats[1]["body"] == {"agent_id": "agent-1", "running_executions": ["eid-1"]}


def test_retry_after_reregister_uses_new_agent_id(go):
    """agent 行被删后重注册会拿到新 agent_id：重试的请求体必须用新 agent_id 重建。"""
    ids = iter(["agent-old", "agent-new"])
    tokens = iter(["token-1", "token-2"])
    client = new_client(
        go,
        **{
            REGISTER: lambda req: (200, {"agent_id": next(ids), "agent_token": next(tokens)}, {}),
            HEARTBEAT: lambda req: (
                (401, {"error": "agent token 无效"}, {})
                if req["body"]["agent_id"] == "agent-old"
                else (200, {"lease_ok": [], "cancel_requested": []}, {})
            ),
        },
    )

    client.heartbeat([])

    assert [r["body"] for r in go.to(HEARTBEAT)] == [
        {"agent_id": "agent-old", "running_executions": []},
        {"agent_id": "agent-new", "running_executions": []},
    ]


def test_two_threads_401_on_same_token_share_one_reregister(go):
    both_sent = threading.Barrier(2, timeout=10)

    def heartbeat(req):
        if token_of(req) == "token-1":
            both_sent.wait()  # 两个线程都拿着旧 token 发出请求后才一起放行 401
            return 401, {"error": "agent token 无效"}, {}
        return 200, {"lease_ok": [], "cancel_requested": []}, {}

    register_calls = rotating_register(go)
    client = new_client(go, **{HEARTBEAT: heartbeat})

    results, errors = run_in_threads(lambda: client.heartbeat([]), 2)

    assert errors == []
    assert results == [HeartbeatResult([], [])] * 2
    assert len(register_calls) == 2  # 首次 + 一次换发；两个线程同时 401 不换发两次


def test_401_after_still_401_raises_unauthorized(go):
    register_calls = rotating_register(go)
    client = new_client(go, **{HEARTBEAT: lambda req: (401, {"error": "agent token 无效"}, {})})

    with pytest.raises(Unauthorized) as excinfo:
        client.heartbeat([])

    assert excinfo.value.status == 401
    assert len(register_calls) == 2
    assert len(go.to(HEARTBEAT)) == 2  # 只重试一次


def test_authed_call_before_register_raises(go):
    with pytest.raises(RuntimeError):
        ControlClient(go.base_url).heartbeat([])
    assert go.requests == []


# ---------- 传输纪律 ----------


def test_redirect_is_not_followed(go):
    client = ControlClient(go.base_url)
    go.on(REGISTER, lambda req: (307, None, {"Location": "/api/v1/elsewhere"}))
    go.on("/api/v1/elsewhere", lambda req: (200, {"agent_id": "x", "agent_token": "y"}, {}))

    with pytest.raises(ControlPlaneError) as excinfo:
        client.register("brp-agent", "0.1.0")

    assert excinfo.value.status == 307
    assert len(go.to(REGISTER)) == 1
    assert go.to("/api/v1/elsewhere") == []


def test_environment_proxy_is_ignored(go, monkeypatch):
    dead_port = free_port()
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"):
        monkeypatch.setenv(name, f"http://127.0.0.1:{dead_port}")
    for name in ("NO_PROXY", "no_proxy"):
        monkeypatch.delenv(name, raising=False)

    assert ControlClient(go.base_url).register("brp-agent", "0.1.0") == "agent-1"


def test_request_times_out(go):
    def slow(req):
        time.sleep(1.0)
        return 200, {"agent_id": "agent-1", "agent_token": "token-1"}, {}

    go.on(REGISTER, slow)
    started = time.monotonic()

    with pytest.raises(ControlPlaneError) as excinfo:
        ControlClient(go.base_url, timeout_s=0.2).register("brp-agent", "0.1.0")

    assert excinfo.value.status is None
    assert time.monotonic() - started < 0.9


def test_one_session_per_thread(go, monkeypatch):
    created = []
    used = []  # (线程 id, Session id)
    real_session = requests.Session

    class CountingSession(real_session):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            created.append(self)

        def post(self, *args, **kwargs):
            used.append((threading.get_ident(), id(self)))
            return super().post(*args, **kwargs)

    monkeypatch.setattr(client_module.requests, "Session", CountingSession)
    client = ControlClient(go.base_url)
    client.register("brp-agent", "0.1.0")
    client.heartbeat([])
    client.heartbeat([])
    run_in_threads(lambda: [client.heartbeat([]), client.heartbeat([])], 2)

    sessions_by_thread = {}
    for thread_id, session_id in used:
        sessions_by_thread.setdefault(thread_id, set()).add(session_id)
    # 同一线程的多次请求复用同一个 Session（不是每次调用新建）
    assert all(len(ids) == 1 for ids in sessions_by_thread.values())
    # 主线程 + 两个工作线程 = 3 个线程，Session 互不相同（不跨线程共享）
    assert len(sessions_by_thread) == 3
    assert len({next(iter(ids)) for ids in sessions_by_thread.values()}) == 3
    assert all(session.trust_env is False for session in created)


# ---------- fresh 评审补强（2026-11-26） ----------


def test_missing_field_error_names_keys_without_echoing_the_body(go):
    """报错只列键名、不带响应体原文：注册响应若缺 agent_id 却带了 agent_token，明文 token 会经日志落盘。"""
    client = ControlClient(go.base_url)
    go.on(REGISTER, lambda req: (200, {"agent_token": "secret-token-value"}, {}))

    with pytest.raises(ControlPlaneError) as excinfo:
        client.register("brp-agent", "0.1.0")

    assert "'agent_id'" in str(excinfo.value)
    assert "agent_token" in str(excinfo.value)  # 列出实际有哪些键，便于定位
    assert "secret-token-value" not in str(excinfo.value)


@pytest.mark.parametrize(
    ("field", "bad"),
    [
        ("task_id", 12),
        ("execution_id", None),
        ("scenario_key", ["x"]),
        ("params_snapshot", None),  # Go 侧快照解码失败时 params_snapshot 就是 null
        ("params_snapshot", []),
    ],
)
def test_claim_field_with_wrong_type_is_a_control_plane_error(go, field, bad):
    """类型不符按"响应不符协议"处理（领取循环会退避），而不是把 None 带进执行器才炸。"""
    body = {
        "task_id": "12",
        "execution_id": "eid-1",
        "attempt": 1,
        "scenario_key": "normal_delivery",
        "params_snapshot": {},
    }
    body[field] = bad
    client = new_client(go, **{CLAIM: lambda req: (200, body, {})})

    with pytest.raises(ControlPlaneError) as excinfo:
        client.claim()

    assert excinfo.value.status == 200
    assert repr(field) in str(excinfo.value)


def test_heartbeat_array_with_wrong_type_is_a_control_plane_error(go):
    client = new_client(go, **{HEARTBEAT: lambda req: (200, {"lease_ok": None, "cancel_requested": []}, {})})

    with pytest.raises(ControlPlaneError, match="'lease_ok'"):
        client.heartbeat([])


# ---------- 第 40 天：events / result 回传 ----------


def test_send_event_wire_and_202(go):
    client = new_client(go, **{"/api/v1/executions/eid-1/events": lambda req: (202, None, {})})

    client.send_event("eid-1", 3, "STATUS", {"substate": "PREPARING"})

    (sent,) = go.to("/api/v1/executions/eid-1/events")
    assert token_of(sent) == "token-1"
    assert sent["body"]["seq"] == 3
    assert sent["body"]["type"] == "STATUS"
    assert sent["body"]["payload"] == {"substate": "PREPARING"}
    ts = sent["body"]["ts"]
    datetime.fromisoformat(ts)  # RFC3339 带时区
    assert ts.endswith("+00:00") and "T" in ts


def test_send_event_duplicate_seq_is_accepted(go):
    """重复 seq 由 Go 幂等去重（§2.4）——Agent 侧同样按 202 处理，不本地去重。"""
    client = new_client(go, **{"/api/v1/executions/eid-1/events": lambda req: (202, None, {})})

    client.send_event("eid-1", 0, "STATUS", {"substate": "CLAIMED"})
    client.send_event("eid-1", 0, "STATUS", {"substate": "CLAIMED"})

    assert len(go.to("/api/v1/executions/eid-1/events")) == 2


def test_send_result_wire_and_200(go):
    body = {"attempt": 1, "status": "COMPLETED", "summary": {}, "reconciliation": {}, "artifacts": []}
    client = new_client(go, **{"/api/v1/executions/eid-1/result": lambda req: (200, {"status": "ok"}, {})})

    client.send_result("eid-1", body)

    (sent,) = go.to("/api/v1/executions/eid-1/result")
    assert token_of(sent) == "token-1"
    assert sent["body"] == body


def test_send_result_error_mapping(go):
    client = new_client(
        go, **{"/api/v1/executions/eid-1/result": lambda req: (400, {"error": "结果不合法"}, {})}
    )

    with pytest.raises(ControlPlaneError) as excinfo:
        client.send_result("eid-1", {"attempt": 1})

    assert excinfo.value.status == 400
