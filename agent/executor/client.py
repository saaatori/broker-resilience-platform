"""executor/client.py Go 控制面 HTTP 客户端（协议 v1.1 §2.1~§2.3）：注册 / 心跳 / 领取。

requests 封装（工程目录 §3.2）。传输纪律（逐日计划 2026-11-24 裁决）：
- 每个请求显式带超时——requests 没有会话级默认值，漏一处就是无限等；
- Session.trust_env = False——不读环境/系统代理（go:8080 是 compose 服务名，排障 #3 同族）；
- 不跟随重定向——307/308 会把 X-Agent-Token 与请求体原样带到别处；
- Session 按线程隔离（thread-local）——主线程 / 心跳线程 / 各执行线程各用各的连接池。
身份（agent_id + token）全线程共享；401 时单飞重注册后重试一次。
"""

import threading
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

import requests

DEFAULT_BASE_URL = "http://go:8080/api/v1"
REQUEST_TIMEOUT_S = 5.0


class ControlPlaneError(Exception):
    """控制面请求失败。status 为 HTTP 状态码；网络层失败（连不上/超时）为 None。"""

    def __init__(self, message: str, status: int | None = None):
        super().__init__(message)
        self.status = status


class Unauthorized(ControlPlaneError):
    """401 且重注册后重试仍 401。"""


class LeaseLost(ControlPlaneError):
    """410：Agent 级租约失效（协议 §3），须中止全部在跑执行。"""


class ClaimConflict(ControlPlaneError):
    """409：领取冲突（防御性保留），退避后重试。"""


@dataclass(frozen=True)
class HeartbeatResult:
    lease_ok: list[str]
    cancel_requested: list[str]


@dataclass(frozen=True)
class ClaimedTask:
    task_id: str  # 数据库 bigint 的十进制字符串（协议 §2.3）
    execution_id: str
    attempt: int
    scenario_key: str
    params_snapshot: dict


def _error(path: str, status: int, text: str) -> ControlPlaneError:
    """按协议 §3 把状态码映射成异常；未列出的状态码一律是基类（由 status 区分 4xx/5xx）。"""
    message = f"POST {path} → HTTP {status}: {text[:200]}"
    special = {401: Unauthorized, 409: ClaimConflict, 410: LeaseLost}
    return special.get(status, ControlPlaneError)(message, status)


def _field(data: dict, key: str, expected: type | None = None) -> Any:
    """取必填字段（可校验类型）；缺失或类型不符都是协议违约，报成 ControlPlaneError 而不是让异常炸掉调用线程。
    报错只列键名与类型，不带响应体原文——注册响应里可能有明文 token，会经日志落盘。"""
    try:
        value = data[key]
    except KeyError:
        raise ControlPlaneError(f"响应缺少字段 {key!r}（实际键：{sorted(data)}）", 200) from None
    if expected is not None and not isinstance(value, expected):
        raise ControlPlaneError(
            f"响应字段 {key!r} 类型不符协议：应为 {expected.__name__}，实际 {type(value).__name__}", 200
        )
    return value


class ControlClient:
    """协议 §2.1~§2.3 三个端点。Session 按线程隔离，本对象可跨线程使用。"""

    def __init__(self, base_url: str = DEFAULT_BASE_URL, timeout_s: float = REQUEST_TIMEOUT_S) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout_s = timeout_s
        self._local = threading.local()
        self._refresh_lock = threading.Lock()  # 串行化重注册；等锁期间若已换发则不重复换发
        self._register_body: dict | None = None
        self._identity: tuple[str, str] | None = None  # (agent_id, agent_token)，整体赋值保证两者一致

    @property
    def agent_id(self) -> str:
        return self._identity[0]

    def register(self, hostname: str, version: str, max_concurrent: int = 1) -> str:
        """注册/重注册（§2.1）：同 hostname 复用 agent_id、换发新 token。返回 agent_id。"""
        body = {"hostname": hostname, "version": version, "max_concurrent": max_concurrent}
        path = "/agent/register"
        data = self._object(self._post(path, body), path)
        self._register_body = body
        self._identity = (_field(data, "agent_id", str), _field(data, "agent_token", str))
        return self.agent_id

    def heartbeat(self, running_executions: list[str]) -> HeartbeatResult:
        """心跳兼取消轮询（§2.2）：双数组原样返回，差集语义由 loop 判定。"""
        path = "/agent/heartbeat"
        resp = self._authed_post(
            path, lambda: {"agent_id": self.agent_id, "running_executions": list(running_executions)}
        )
        data = self._object(resp, path)
        return HeartbeatResult(_field(data, "lease_ok", list), _field(data, "cancel_requested", list))

    def claim(self) -> ClaimedTask | None:
        """原子领取一个任务（§2.3）；无任务（204）返回 None。"""
        path = "/agent/tasks/claim"
        resp = self._authed_post(path, lambda: {"agent_id": self.agent_id})
        if resp.status_code == 204:
            return None
        data = self._object(resp, path)
        # 类型在这里就卡住：params_snapshot 为 null（Go 侧快照解码失败时的返回）若漏过去，
        # 要到第 39 天的 runner 里才炸，而且那时执行已被领取、只能靠租约过期收口
        return ClaimedTask(
            task_id=_field(data, "task_id", str),
            execution_id=_field(data, "execution_id", str),
            attempt=_field(data, "attempt"),
            scenario_key=_field(data, "scenario_key", str),
            params_snapshot=_field(data, "params_snapshot", dict),
        )

    def send_event(self, eid: str, seq: int, event_type: str, payload: dict) -> None:
        """执行事件上报（§2.4）：202（含重复 seq 静默去重）。ts=调用时刻（=事件发生时刻）。"""
        path = f"/executions/{eid}/events"
        body = {
            "seq": seq,
            "ts": datetime.now(UTC).isoformat(),
            "type": event_type,
            "payload": payload,
        }
        resp = self._authed_post(path, lambda: body)
        if resp.status_code != 202:
            raise _error(path, resp.status_code, resp.text)

    def send_result(self, eid: str, body: dict) -> None:
        """结果回传（§2.5）：200（first-wins 幂等，重复回传同样 200）。"""
        path = f"/executions/{eid}/result"
        resp = self._authed_post(path, lambda: dict(body))
        if resp.status_code != 200:
            raise _error(path, resp.status_code, resp.text)

    # ---------- 传输 ----------

    def _session(self) -> requests.Session:
        session = getattr(self._local, "session", None)
        if session is None:
            session = self._local.session = requests.Session()
            session.trust_env = False
        return session

    def _post(self, path: str, body: dict, token: str | None = None) -> requests.Response:
        """唯一出口：每个请求都经这里，超时与不跟随重定向没有漏网的调用点。"""
        headers = {"X-Agent-Token": token} if token else None
        try:
            return self._session().post(
                self._base_url + path,
                json=body,
                headers=headers,
                timeout=self._timeout_s,
                allow_redirects=False,
            )
        except requests.RequestException as e:
            raise ControlPlaneError(f"POST {path} 请求失败: {e}") from e

    def _authed_post(self, path: str, make_body: Callable[[], dict]) -> requests.Response:
        """带 token 的请求：401 则重注册（单飞）后重试一次。make_body 在发送时才构造——
        重注册后 agent_id 可能已变（agent 行被删），重试的请求体要用新值重建。"""
        if self._identity is None:
            raise RuntimeError("尚未注册：先调用 register()")
        stale_token = self._identity[1]
        resp = self._post(path, make_body(), token=stale_token)
        if resp.status_code == 401:
            with self._refresh_lock:
                if self._identity[1] == stale_token:
                    self.register(**self._register_body)
            resp = self._post(path, make_body(), token=self._identity[1])
        return resp

    @staticmethod
    def _object(resp: requests.Response, path: str) -> dict:
        if resp.status_code != 200:
            raise _error(path, resp.status_code, resp.text)
        try:
            data = resp.json()
        except ValueError:
            data = None
        if not isinstance(data, dict):
            raise ControlPlaneError(f"POST {path} → HTTP 200 但响应不是 JSON 对象: {resp.text[:200]}", 200)
        return data
