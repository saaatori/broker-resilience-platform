"""executor/loop.py 单测（第 38 天）：FakeClient + 可阻塞的假执行器，不睡真实周期。

间隔参数缩到 20~50ms；等待一律「轮询到条件成立」并带截止时间。
"""

import logging
import threading
import time

import pytest

from executor.client import (
    ClaimConflict,
    ClaimedTask,
    ControlPlaneError,
    HeartbeatResult,
    LeaseLost,
    Unauthorized,
)
from executor.loop import ABORT_CANCEL, ABORT_LEASE_LOST, AgentLoop, Backoff

FAST = {
    "heartbeat_interval_s": 0.02,
    "heartbeat_retry_s": 0.02,
    "claim_interval_s": 0.02,
    "backoff_max_s": 0.08,
}


# ---------- Fakes ----------


class FakeClient:
    """假控制面客户端：按脚本返回/抛错，并记录每次调用（带单调时钟，用于断言节奏）。"""

    def __init__(self, *, register_errors=(), heartbeat_script=(), claim_script=()):
        self.register_errors = list(register_errors)  # 注册失败脚本：先抛完这些再成功
        self.heartbeat_script = list(heartbeat_script)  # 每次心跳弹出一条：结果或异常
        self.claim_script = list(claim_script)  # 每次 claim 弹出一条：任务 / None / 异常
        self.register_calls = []
        self.agent_id = "agent-1"  # 与 ControlClient.agent_id 对齐（loop 注册后要读）
        self.heartbeats = []  # (时刻, 上报的 eid 列表, 结果或异常)
        self.claims = []  # (时刻, 结果或异常)
        self.on_heartbeat = None  # 测试钩子：请求发出后、响应返回前调用

    def register(self, hostname, version, max_concurrent=1):
        self.register_calls.append((hostname, version, max_concurrent))
        if self.register_errors:
            raise self.register_errors.pop(0)
        return "agent-1"

    def heartbeat(self, running_executions):
        if self.on_heartbeat is not None:
            self.on_heartbeat(list(running_executions))
        # 脚本耗尽 = 健康控制面：上报的执行全部续租
        outcome = self.heartbeat_script.pop(0) if self.heartbeat_script else None
        if outcome is None:
            outcome = HeartbeatResult(lease_ok=list(running_executions), cancel_requested=[])
        self.heartbeats.append((time.monotonic(), list(running_executions), outcome))
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    def claim(self):
        outcome = self.claim_script.pop(0) if self.claim_script else None
        self.claims.append((time.monotonic(), outcome))
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


class BlockingExecute:
    """假执行器：每个执行阻塞到被放行；记录 (task, abort, 线程名)。"""

    def __init__(self):
        self.calls = []  # (task, AbortSignal, thread name)
        self.started = {}  # eid -> AbortSignal
        self.error_for = set()  # 这些 eid 一进来就抛
        self._release = {}

    def __call__(self, task, abort):
        self.calls.append((task, abort, threading.current_thread().name))
        self.started[task.execution_id] = abort
        if task.execution_id in self.error_for:
            raise RuntimeError("boom")
        self._release.setdefault(task.execution_id, threading.Event()).wait(timeout=10)

    def finish(self, eid):
        self._release.setdefault(eid, threading.Event()).set()


def task(eid, n=1):
    return ClaimedTask(
        task_id=str(n), execution_id=eid, attempt=1, scenario_key="normal_delivery", params_snapshot={}
    )


def wait_until(cond, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return True
        time.sleep(0.005)
    return bool(cond())


def make_loop(client, execute, **overrides):
    return AgentLoop(client, execute, hostname="brp-agent", version="0.1.0", **{**FAST, **overrides})


@pytest.fixture
def run_loop():
    started = []

    def start(loop):
        thread = threading.Thread(target=loop.run, name="loop", daemon=True)
        thread.start()
        started.append((thread, loop))
        return thread

    yield start
    for thread, loop in started:  # 用例失败也保证收尾，不留后台线程
        loop.stop()
        thread.join(timeout=5)


def start_two(run_loop, **loop_kwargs):
    """领起两个在跑执行（max_concurrent=2），等两个都进入执行器。"""
    client = FakeClient(claim_script=[task("eid-a"), task("eid-b")])
    execute = BlockingExecute()
    loop = make_loop(client, execute, max_concurrent=2, **loop_kwargs)
    run_loop(loop)
    assert wait_until(lambda: len(execute.calls) == 2)
    return client, execute, loop


# ---------- Backoff ----------


def test_backoff_doubles_caps_then_resets():
    backoff = Backoff(5, 30)

    assert [backoff.next() for _ in range(5)] == [5, 10, 20, 30, 30]

    backoff.reset()
    assert backoff.next() == 5


def test_backoff_max_never_drops_below_initial():
    assert [Backoff(30, 5).next() for _ in range(3)] == [30, 30, 30]


# ---------- 注册与心跳 ----------


def test_registers_before_first_beat_and_first_beat_is_immediate(run_loop):
    client, execute = FakeClient(), BlockingExecute()
    loop = make_loop(client, execute, heartbeat_interval_s=30)  # 周期拉长：首发若等一个周期，2s 内等不到

    run_loop(loop)

    assert wait_until(lambda: len(client.heartbeats) >= 1, timeout=2)
    assert client.register_calls == [("brp-agent", "0.1.0", 1)]
    assert client.heartbeats[0][1] == []  # 首发上报空列表


def test_startup_register_errors_are_retried_until_success(run_loop):
    client = FakeClient(register_errors=[ControlPlaneError("go 未就绪", None)] * 2)
    execute = BlockingExecute()

    run_loop(make_loop(client, execute))

    assert wait_until(lambda: len(client.register_calls) == 3)
    assert wait_until(lambda: len(client.heartbeats) >= 2)
    assert len(client.register_calls) == 3  # 成功后不再注册


# ---------- 领取与并发额度 ----------


def test_claimed_execution_runs_in_own_thread_and_is_reported_until_it_finishes(run_loop):
    client = FakeClient(claim_script=[task("eid-1", n=7)])
    execute = BlockingExecute()
    loop = make_loop(client, execute)

    run_loop(loop)

    assert wait_until(lambda: len(execute.calls) == 1)
    (sent, _abort, thread_name) = execute.calls[0]
    assert (sent.task_id, sent.execution_id) == ("7", "eid-1")
    assert thread_name.startswith("exec-") and thread_name not in ("heartbeat", "loop")
    assert loop.running_ids() == ["eid-1"]
    assert wait_until(lambda: ["eid-1"] in [running for _, running, _ in client.heartbeats])

    execute.finish("eid-1")

    assert wait_until(lambda: loop.running_ids() == [])
    n = len(client.heartbeats)
    assert wait_until(lambda: len(client.heartbeats) >= n + 3)
    assert all(running == [] for _, running, _ in client.heartbeats[n:])  # 收尾后不再上报


def test_no_claim_at_capacity_resumes_when_slot_frees(run_loop):
    tasks = [task(f"eid-{i}", n=i) for i in range(5)]
    client = FakeClient(claim_script=tasks)
    execute = BlockingExecute()
    loop = make_loop(client, execute, max_concurrent=2)

    run_loop(loop)

    assert wait_until(lambda: len(execute.calls) == 2)
    # 额度已满：拿心跳当时钟，过 ≥10 个领取周期（0.2s）——期间一次 claim 都不该发
    n = len(client.heartbeats)
    assert wait_until(lambda: len(client.heartbeats) >= n + 10)
    assert len(client.claims) == 2

    execute.finish("eid-0")

    assert wait_until(lambda: len(execute.calls) == 3)
    assert [t.execution_id for t, _, _ in execute.calls] == ["eid-0", "eid-1", "eid-2"]


def test_execute_exception_untracks_and_loop_continues(run_loop, caplog):
    caplog.set_level(logging.ERROR, logger="brp-agent")
    client = FakeClient(claim_script=[task("eid-1"), task("eid-2")])
    execute = BlockingExecute()
    execute.error_for.add("eid-1")
    loop = make_loop(client, execute)

    with caplog.at_level(logging.ERROR, logger="brp-agent"):
        run_loop(loop)
        assert wait_until(lambda: len(execute.calls) == 2)

    assert loop.running_ids() == ["eid-2"]  # 抛异常的执行不占额度
    assert any("eid-1" in r.getMessage() for r in caplog.records)  # 日志带 execution_id


# ---------- 取消 / 租约失效响应 ----------


def test_cancel_requested_aborts_only_that_execution(run_loop):
    client, execute, _ = start_two(run_loop)
    client.heartbeat_script.append(HeartbeatResult(lease_ok=["eid-b"], cancel_requested=["eid-a"]))

    assert wait_until(
        lambda: any(o.cancel_requested == ["eid-a"] for _, _, o in client.heartbeats if not isinstance(o, Exception))
    )
    assert wait_until(lambda: execute.started["eid-a"].is_set())
    assert execute.started["eid-a"].reason == ABORT_CANCEL
    assert not execute.started["eid-b"].is_set()


def test_missing_from_both_arrays_aborts_as_lease_lost(run_loop):
    client, execute, _ = start_two(run_loop)
    client.heartbeat_script.append(HeartbeatResult(lease_ok=["eid-b"], cancel_requested=[]))

    assert wait_until(
        lambda: any(o.lease_ok == ["eid-b"] for _, _, o in client.heartbeats if not isinstance(o, Exception))
    )
    assert wait_until(lambda: execute.started["eid-a"].is_set())
    assert execute.started["eid-a"].reason == ABORT_LEASE_LOST
    assert not execute.started["eid-b"].is_set()


def test_execution_tracked_after_heartbeat_snapshot_is_not_aborted(run_loop):
    client = FakeClient(claim_script=[task("eid-a")])
    execute = BlockingExecute()
    loop = make_loop(client, execute)

    def late_start(running):  # eid-a 在跑的这次心跳：请求在途期间（快照之后）又起了一个执行
        if not running:  # 首发心跳早于领取：等下一次
            return
        assert running == ["eid-a"]
        client.on_heartbeat = None
        client.heartbeat_script.append(HeartbeatResult(lease_ok=[], cancel_requested=[]))
        loop._start_execution(task("eid-b"))

    client.on_heartbeat = late_start
    # 这次响应按旧快照生成：eid-a 已不在 lease_ok，eid-b 根本不在响应里

    run_loop(loop)

    assert wait_until(lambda: len(execute.calls) == 2)
    assert wait_until(lambda: execute.started["eid-a"].is_set())
    assert not execute.started["eid-b"].is_set()  # 不在快照里 → 不受这次响应影响


def test_agent_level_lease_lost_aborts_all_and_keeps_beating(run_loop):
    client, execute, _ = start_two(run_loop)
    client.heartbeat_script.append(LeaseLost("410", 410))

    assert wait_until(lambda: execute.started["eid-a"].is_set() and execute.started["eid-b"].is_set())
    assert execute.started["eid-a"].reason == ABORT_LEASE_LOST
    assert execute.started["eid-b"].reason == ABORT_LEASE_LOST
    n = len(client.heartbeats)  # 心跳线程不因 410 退出
    assert wait_until(lambda: len(client.heartbeats) >= n + 5)


def test_claim_level_lease_lost_aborts_all_running(run_loop):
    # 一个执行在跑 + 空余额度：下一次 claim 才会发出并拿到 Agent 级 410
    client = FakeClient(claim_script=[task("eid-a"), LeaseLost("410", 410)])
    execute = BlockingExecute()

    run_loop(make_loop(client, execute, max_concurrent=2))

    assert wait_until(lambda: any(isinstance(o, LeaseLost) for _, o in client.claims))
    assert wait_until(lambda: execute.started["eid-a"].is_set())
    assert execute.started["eid-a"].reason == ABORT_LEASE_LOST


@pytest.mark.parametrize(
    "error",
    [ControlPlaneError("go 500", 500), ControlPlaneError("连不上", None), KeyError("处理逻辑 bug")],
    ids=["5xx", "transport", "bug"],
)
def test_heartbeat_error_does_not_kill_thread_nor_abort_executions(run_loop, error):
    client, execute, _ = start_two(run_loop)
    client.heartbeat_script.extend([error] * 3)

    n = len(client.heartbeats)
    assert wait_until(lambda: len(client.heartbeats) >= n + 3 + 5)  # 报错 3 次后照常继续跳
    assert not execute.started["eid-a"].is_set()  # 心跳请求失败 ≠ 租约失效：只有成功响应的差集 / 410 才中止
    assert not execute.started["eid-b"].is_set()


# ---------- 领取节奏 ----------


@pytest.mark.parametrize(
    "error",
    [
        ClaimConflict("409", 409),
        LeaseLost("410", 410),
        ControlPlaneError("go 500", 500),
        ControlPlaneError("连不上", None),
        ControlPlaneError("响应缺少字段 'task_id'", 200),
    ],
    ids=["409", "410", "5xx", "transport", "malformed-200"],
)
def test_transient_claim_errors_back_off_then_recover(run_loop, error):
    client = FakeClient(claim_script=[error] * 3 + [task("eid-1")])
    execute = BlockingExecute()

    run_loop(make_loop(client, execute, claim_interval_s=0.05, backoff_max_s=0.2))

    assert wait_until(lambda: len(execute.calls) == 1)
    gaps = [later - earlier for (earlier, _), (later, _) in zip(client.claims, client.claims[1:])]
    # 退避 0.05 → 0.1 → 0.2（封顶）；只断下界——调度抖动只会让间隔更长
    assert gaps[0] >= 0.045
    assert gaps[1] >= 0.095
    assert gaps[2] >= 0.195


@pytest.mark.parametrize(
    "error",
    [ControlPlaneError("请求参数不合法", 400), Unauthorized("重注册后仍 401", 401)],
    ids=["400", "401-after-retry"],
)
def test_other_4xx_keeps_idle_cadence_without_backoff(run_loop, error):
    client = FakeClient(claim_script=[error] * 6)
    execute = BlockingExecute()

    run_loop(make_loop(client, execute, claim_interval_s=0.05, backoff_max_s=0.2))

    assert wait_until(lambda: len(client.claims) == 6)
    elapsed = client.claims[-1][0] - client.claims[0][0]
    assert elapsed < 0.6  # 5 次空闲等待各 0.05s；若按退避走则 ≥0.75s


# ---------- 停止 ----------


def test_stop_unblocks_long_waits_and_run_returns(run_loop):
    client = FakeClient()  # claim 永远 204：循环停在空闲等待里
    loop = make_loop(client, BlockingExecute(), heartbeat_interval_s=30, claim_interval_s=30)

    thread = run_loop(loop)
    assert wait_until(lambda: len(client.heartbeats) >= 1, timeout=2)

    loop.stop()

    thread.join(timeout=5)
    assert not thread.is_alive()
    n = len(client.heartbeats)
    time.sleep(0.05)
    assert len(client.heartbeats) == n  # 停止后不再有心跳


# ---------- fresh 评审补强（2026-11-26） ----------


def test_instant_executor_never_leaks_a_slot(run_loop):
    """执行器不阻塞、不打日志时，必须先把执行登记进在跑表再起执行线程。
    登记若晚于线程启动，线程跑完就把自己从表里摘掉，之后登记进去的是幽灵——占死唯一名额，Agent 再也不领。"""
    tasks = [task(f"eid-{i}", n=i) for i in range(50)]
    client = FakeClient(claim_script=tasks)
    done = []
    loop = make_loop(client, lambda t, abort: done.append(t.execution_id), max_concurrent=1)

    run_loop(loop)

    assert wait_until(lambda: len(done) == 50)  # 登记晚于线程启动时：只领到 1 个就卡死
    assert loop.running_ids() == []


def test_agent_level_lease_lost_aborts_execution_tracked_after_snapshot_too(run_loop):
    """Agent 级 410 是对整个 Agent 的裁决，不是逐执行的：快照之后才登记的执行也要一并中止，不必等下一个周期。"""
    client = FakeClient(claim_script=[task("eid-a")])
    execute = BlockingExecute()
    loop = make_loop(client, execute)

    def late_start(running):  # eid-a 在跑的这次心跳：请求在途期间（快照之后）又起了一个执行
        if not running:  # 首发心跳早于领取：等下一次
            return
        assert running == ["eid-a"]
        client.on_heartbeat = None
        client.heartbeat_script.append(LeaseLost("410", 410))
        loop._start_execution(task("eid-b"))

    client.on_heartbeat = late_start

    run_loop(loop)

    assert wait_until(lambda: len(execute.calls) == 2)
    assert wait_until(lambda: execute.started["eid-a"].is_set())
    assert execute.started["eid-b"].is_set()
    assert execute.started["eid-b"].reason == ABORT_LEASE_LOST


def test_first_abort_reason_is_not_overwritten(run_loop):
    """同一执行先被取消、随后又落进差集：中止原因保持先到的 cancel（关闭路径按原因分流，不能被后到的覆盖）。"""
    client, execute, _ = start_two(run_loop)
    second = HeartbeatResult(lease_ok=["eid-b"], cancel_requested=[])
    client.heartbeat_script.append(HeartbeatResult(lease_ok=["eid-b"], cancel_requested=["eid-a"]))
    client.heartbeat_script.append(second)

    # 等第二次响应被消费、且其后至少又跳了一次：心跳线程是串行的，下一次心跳开始即说明上一次已处理完
    assert wait_until(lambda: any(o is second for _, _, o in client.heartbeats))
    consumed_at = next(i for i, (_, _, o) in enumerate(client.heartbeats) if o is second)
    assert wait_until(lambda: len(client.heartbeats) > consumed_at + 1)
    assert execute.started["eid-a"].is_set()
    assert execute.started["eid-a"].reason == ABORT_CANCEL


class LeaseKeepingClient(FakeClient):
    """按 Go 的租约规则（heartbeat.go judgeExec）模拟控制面：租约未过期的执行才进 lease_ok，
    lease_ok 非空才续租；租约已过期的执行不出现在任何数组里（即差集 → Agent 判 lease_lost）。
    时间原点取在领取那一刻；down_windows 是控制面不可用的时间窗（相对原点，左开右闭）。"""

    def __init__(self, lease_s, down_windows):
        super().__init__(claim_script=[task("eid-1")])
        self.lease_s = lease_s
        self.down_windows = down_windows
        self.t0 = None
        self.lease_until = None

    def claim(self):
        outcome = super().claim()
        if outcome is not None and self.t0 is None:
            self.t0 = time.monotonic()
            self.lease_until = self.lease_s
        return outcome

    def heartbeat(self, running_executions):
        if self.t0 is None:
            return super().heartbeat(running_executions)  # 领取之前：没有租约要续
        now = time.monotonic() - self.t0
        if any(start < now <= end for start, end in self.down_windows):
            self.heartbeats.append((time.monotonic(), list(running_executions), "down"))
            raise ControlPlaneError("连不上", None)
        ok = [eid for eid in running_executions if self.lease_until > now]
        if ok:
            self.lease_until = now + self.lease_s
        outcome = HeartbeatResult(lease_ok=ok, cancel_requested=[])
        self.heartbeats.append((time.monotonic(), list(running_executions), outcome))
        return outcome


def test_control_plane_outage_of_two_heartbeat_intervals_does_not_lose_lease(run_loop):
    """租约 45s 名义上够三次心跳失败；若失败后仍等满 15s，尝试落在 15 / 30 / 45s——连续两次失败租约就没了。
    按真实常量缩到 1/50（心跳 0.3s、租约 0.9s、失败后重试 0.04s）：控制面从首个心跳点起不可用 0.4s
    （两个心跳周期之内、租约过期之前恢复），执行不能被判 lease_lost。"""
    client = LeaseKeepingClient(lease_s=0.9, down_windows=[(0.3, 0.7)])
    execute = BlockingExecute()
    loop = make_loop(
        client, execute, heartbeat_interval_s=0.3, heartbeat_retry_s=0.04, claim_interval_s=0.02
    )

    run_loop(loop)

    assert wait_until(lambda: len(execute.calls) == 1)
    time.sleep(1.2)  # 越过租约原定过期的时刻（领取后 0.9s）
    downs = [h for h in client.heartbeats if h[2] == "down"]
    assert len(downs) >= 5  # 不可用窗口内确实失败了多次
    assert not execute.started["eid-1"].is_set()


def test_failed_heartbeat_is_retried_soon_then_cadence_returns_to_normal(run_loop):
    client = FakeClient(heartbeat_script=[ControlPlaneError("连不上", None)])
    # 正常周期拉长到 30s：失败后若仍等满一个周期，0.5s 内等不到第二次心跳
    loop = make_loop(client, BlockingExecute(), heartbeat_interval_s=30, heartbeat_retry_s=0.05)

    run_loop(loop)

    assert wait_until(lambda: len(client.heartbeats) >= 2, timeout=1.5)
    n = len(client.heartbeats)
    time.sleep(0.3)
    assert len(client.heartbeats) == n  # 恢复健康后回到正常周期，不再密集重试


@pytest.mark.parametrize("recovery", [task("eid-1"), None], ids=["task", "204"])
def test_successful_poll_resets_claim_backoff(run_loop, recovery):
    script = [ClaimConflict("409", 409)] * 3 + [recovery, ClaimConflict("409", 409), task("eid-2", n=2)]
    client = FakeClient(claim_script=script)
    execute = BlockingExecute()

    run_loop(make_loop(client, execute, claim_interval_s=0.02, backoff_max_s=0.8, max_concurrent=2))

    assert wait_until(lambda: len(execute.calls) == (2 if recovery is not None else 1))
    gaps = [later - earlier for (earlier, _), (later, _) in zip(client.claims, client.claims[1:])]
    # 连续三次 409 把退避推到 0.16s；一次成功（领到任务或 204）之后复位，再遇 409 只退 0.02s
    assert gaps[2] >= 0.075
    assert gaps[4] < 0.1


def test_run_exception_stops_heartbeat_thread():
    """领取循环因非协议异常退出时，run() 的收尾必须置 stop，否则心跳线程在宿主进程里一直跳。"""
    client = FakeClient(claim_script=[KeyError("领取循环的 bug")])
    loop = make_loop(client, BlockingExecute(), heartbeat_interval_s=0.02)

    with pytest.raises(KeyError):
        loop.run()

    n = len(client.heartbeats)
    time.sleep(0.1)
    assert len(client.heartbeats) == n


def test_max_concurrent_below_one_is_treated_as_one(run_loop):
    client = FakeClient(claim_script=[task("eid-1"), task("eid-2")])
    execute = BlockingExecute()

    run_loop(make_loop(client, execute, max_concurrent=0))

    assert wait_until(lambda: len(execute.calls) == 1)
    assert client.register_calls == [("brp-agent", "0.1.0", 1)]  # 向控制面如实申报 1，而不是 0
    n = len(client.heartbeats)
    assert wait_until(lambda: len(client.heartbeats) >= n + 10)
    assert len(execute.calls) == 1
