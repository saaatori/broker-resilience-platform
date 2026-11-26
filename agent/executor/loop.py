"""executor/loop.py Agent 主循环（M2 第 38 天）：注册 → 心跳线程 + 领取循环 → 取消 / 租约失效响应。

线程模型：
- run() 所在线程跑领取循环：额度未满时 claim，领到即起一个执行线程跑注入的 execute；
- 心跳线程按固定周期上报在跑执行（失败后缩短间隔重试），消费双数组响应（协议 §2.2 差集语义）
  与 Agent 级 410（§3）；
- 执行线程只与「在跑表」（eid → AbortSignal，锁保护）交互。
执行本身（runner / 回传）是第 39、40 天；今日的 execute 由 main 注入占位实现。
"""

import logging
import threading
from collections.abc import Callable

from executor.client import (
    REQUEST_TIMEOUT_S,
    ClaimConflict,
    ClaimedTask,
    ControlPlaneError,
    LeaseLost,
)

HEARTBEAT_INTERVAL_S = 15.0  # 协议 §1 口径，不可擅改
HEARTBEAT_RETRY_S = 2.0  # 心跳失败后的重试间隔（健康时仍是 15s；口径表里没有这一项，见 _heartbeat_loop）
CLAIM_IDLE_INTERVAL_S = 5.0  # 无任务时的轮询间隔（领取延迟 ≤10s 由它保证）
CLAIM_BACKOFF_MAX_S = 30.0  # 409 / 5xx 退避上限

ABORT_CANCEL = "cancel"
ABORT_LEASE_LOST = "lease_lost"

log = logging.getLogger("brp-agent")

Execute = Callable[[ClaimedTask, "AbortSignal"], None]
# Execute 的契约（第 39、40 天接入真实执行器时必须遵守）：
# - execute 一返回，执行就离开在跑表、心跳不再为它续租——events / result / cleanup-ack 的回传
#   必须全部在返回之前做完，否则只剩"上次续租 + 45s"的余量；
# - Go 把子状态已是 DONE / ABORTED 的执行排除在心跳的两个数组之外（heartbeat.go judgeExec）。
#   发出终态 STATUS 之后、execute 返回之前，这次执行会落进差集并收到 lease_lost 中止信号，
#   执行器要把"终态子状态之后的中止"当空操作。


class AbortSignal:
    """执行的中止信号：心跳线程置位，执行线程经 wait()/is_set() 感知；reason 取 ABORT_*。"""

    def __init__(self) -> None:
        self.reason: str | None = None
        self._event = threading.Event()

    def is_set(self) -> bool:
        return self._event.is_set()

    def wait(self, timeout: float | None = None) -> bool:
        return self._event.wait(timeout)

    def _fire(self, reason: str) -> None:
        self.reason = reason  # 先写原因再置位：wait() 返回后读到的 reason 一定就绪
        self._event.set()


class Backoff:
    """指数退避：从 initial_s 起每次翻倍、封顶 max_s；reset() 回到 initial_s。"""

    def __init__(self, initial_s: float, max_s: float) -> None:
        self._initial_s = initial_s
        self._max_s = max(max_s, initial_s)
        self._delay_s = initial_s

    def next(self) -> float:
        delay = self._delay_s
        self._delay_s = min(self._delay_s * 2, self._max_s)
        return delay

    def reset(self) -> None:
        self._delay_s = self._initial_s


class AgentLoop:
    """注册 → 心跳线程 + 领取循环。client 与 execute 经构造注入。"""

    def __init__(
        self,
        client,
        execute: Execute,
        *,
        hostname: str,
        version: str,
        max_concurrent: int = 1,
        heartbeat_interval_s: float = HEARTBEAT_INTERVAL_S,
        heartbeat_retry_s: float = HEARTBEAT_RETRY_S,
        claim_interval_s: float = CLAIM_IDLE_INTERVAL_S,
        backoff_max_s: float = CLAIM_BACKOFF_MAX_S,
    ) -> None:
        self._client = client
        self._execute = execute
        self._hostname = hostname
        self._version = version
        self._max_concurrent = max(1, max_concurrent)
        self._heartbeat_interval_s = heartbeat_interval_s
        self._heartbeat_retry_s = heartbeat_retry_s
        self._claim_interval_s = claim_interval_s
        self._backoff_max_s = backoff_max_s
        self._stop = threading.Event()  # 置位即打断一切等待
        self._lock = threading.Lock()
        self._running: dict[str, AbortSignal] = {}

    def run(self) -> None:
        """阻塞直到 stop()：注册（退避重试直到成功）→ 起心跳线程 → 领取循环。"""
        threading.current_thread().name = "claim-loop"
        if not self._register_with_retry():
            return
        log.info(
            "Agent 已注册：agent_id=%s hostname=%s max_concurrent=%d",
            self._client.agent_id,
            self._hostname,
            self._max_concurrent,
        )
        heartbeat = threading.Thread(target=self._heartbeat_loop, name="heartbeat", daemon=True)
        heartbeat.start()
        try:
            self._claim_loop()
        finally:
            self._stop.set()
            heartbeat.join(timeout=REQUEST_TIMEOUT_S + 1)

    def stop(self) -> None:
        self._stop.set()

    def running_ids(self) -> list[str]:
        with self._lock:
            return list(self._running)

    # ---------- 注册 ----------

    def _register_with_retry(self) -> bool:
        """注册失败按指数退避重试，直到成功或 stop()；返回是否注册成功。"""
        backoff = Backoff(self._claim_interval_s, self._backoff_max_s)
        while not self._stop.is_set():
            try:
                self._client.register(self._hostname, self._version, self._max_concurrent)
                return True
            except ControlPlaneError as e:
                delay = backoff.next()
                log.warning("注册失败，%.0fs 后重试：%s", delay, e)
                self._stop.wait(delay)
        return False

    # ---------- 心跳线程 ----------

    def _heartbeat_loop(self) -> None:
        while not self._stop.is_set():
            try:
                healthy = self._beat()
            except Exception:  # 心跳线程一死租约必过期：任何异常都只记录、下一周期继续
                log.exception("心跳处理异常（下一周期继续）")
                healthy = False
            # 失败后不等满一个周期。租约 45s 只够两次心跳失败：失败后若仍等 15s，尝试落在 15/30/45s，
            # 控制面抖动十几秒在跑执行就会被判 lease_lost。改成 2s 重试后，从上次成功心跳起约 43s 内
            # 恢复都能接上（过期前最后一次来得及的尝试在 43s 左右）——实测口径见开发日志第 38 天。
            self._stop.wait(self._heartbeat_interval_s if healthy else self._heartbeat_retry_s)

    def _beat(self) -> bool:
        """一次心跳。返回控制面是否给出了裁决（含 410）；False 表示请求失败，调用方应尽快重试。"""
        reported = self.running_ids()  # 差集判定只认这份快照：之后才登记的执行不在响应里，不能被误杀
        try:
            result = self._client.heartbeat(reported)
        except LeaseLost:
            # Agent 级 410 是对整个 Agent 的裁决，不是逐执行的：在跑的全部中止，不套用快照规则
            log.warning("Agent 级租约失效（410）：中止全部在跑执行 exec=%s", self.running_ids())
            self._abort_all(ABORT_LEASE_LOST)
            return True
        except ControlPlaneError as e:
            # 请求失败 ≠ 租约失效：只有成功响应的差集与 410 才中止执行
            log.warning("心跳失败（%ss 后重试）exec=%s：%s", self._heartbeat_retry_s, reported, e)
            return False
        lease_ok, cancel = set(result.lease_ok), set(result.cancel_requested)
        for eid in reported:
            if eid in cancel:
                self._abort(eid, ABORT_CANCEL)
            elif eid not in lease_ok:
                self._abort(eid, ABORT_LEASE_LOST)  # 差集语义 = 视同 410（协议 §2.2）
        return True

    # ---------- 领取循环 ----------

    def _claim_loop(self) -> None:
        backoff = Backoff(self._claim_interval_s, self._backoff_max_s)
        while not self._stop.is_set():
            if self._free_slots() == 0:
                self._stop.wait(self._claim_interval_s)  # 额度已满：等执行收尾后再看
                continue
            try:
                claimed = self._client.claim()
            except LeaseLost:
                self._abort_all(ABORT_LEASE_LOST)
                delay = backoff.next()
            except ControlPlaneError as e:
                delay = self._claim_error_delay(e, backoff)
            else:
                if claimed is not None:
                    self._start_execution(claimed)
                    backoff.reset()
                    continue  # 立刻再看额度：max_concurrent > 1 时把额度领满
                delay = self._claim_interval_s  # 204 无任务
                backoff.reset()
            self._stop.wait(delay)

    def _claim_error_delay(self, err: ControlPlaneError, backoff: Backoff) -> float:
        transient = (
            isinstance(err, ClaimConflict)
            or err.status is None
            or err.status < 400
            or err.status >= 500
        )
        if not transient:
            # 其余 4xx（400/404 等）：协议 §1 不重试、记日志告警——按空闲节奏继续轮询
            log.warning("领取请求被拒（不重试）：%s", err)
            return self._claim_interval_s
        # 409 / 5xx / 网络层失败 / 响应不符协议：退避后重试
        log.warning("领取失败（退避后重试）：%s", err)
        return backoff.next()

    def _free_slots(self) -> int:
        with self._lock:
            return self._max_concurrent - len(self._running)

    def _start_execution(self, task: ClaimedTask) -> None:
        # 先登记、后起线程，次序不能反：执行器可能瞬间返回，线程先跑完再登记的话，登记进去的是幽灵——
        # 占住并发名额却永远不会被摘掉，Agent 从此不再领取
        signal = AbortSignal()
        with self._lock:
            self._running[task.execution_id] = signal
        log.info(
            "已领取：task=%s exec=%s attempt=%s scenario=%s",
            task.task_id,
            task.execution_id,
            task.attempt,
            task.scenario_key,
        )
        threading.Thread(
            target=self._run_execution,
            args=(task, signal),
            name=f"exec-{task.execution_id[:8]}",
            daemon=True,
        ).start()

    def _run_execution(self, task: ClaimedTask, signal: AbortSignal) -> None:
        try:
            self._execute(task, signal)
        except Exception:  # 执行线程的异常不能拖垮 Agent；未回传的执行由租约过期收口
            log.exception("执行线程异常退出：task=%s exec=%s", task.task_id, task.execution_id)
        finally:
            with self._lock:  # 无论成败都释放额度，否则占住并发名额后再也不领
                self._running.pop(task.execution_id, None)

    # ---------- 中止 ----------

    def _abort(self, eid: str, reason: str) -> None:
        with self._lock:
            signal = self._running.get(eid)
            if signal is None or signal.is_set():
                return
            signal._fire(reason)
        log.warning("执行被中止：exec=%s reason=%s", eid, reason)

    def _abort_all(self, reason: str) -> None:
        for eid in self.running_ids():
            self._abort(eid, reason)
