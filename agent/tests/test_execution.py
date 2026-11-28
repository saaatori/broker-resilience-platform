"""executor/execution.py 单测（第 40 天）：结果体组装 + execute 编排。

组装是纯函数（本文件前段）；execute 编排用 fake 注入（client/factory/runner/docker），
不发真实网络与 docker 调用。
"""

import json
import threading
from pathlib import Path
from types import SimpleNamespace

import pytest
from agent.executor import execution

from executor.client import ClaimedTask, ControlPlaneError
from runner.pytest_runner import RunnerOutcome

SCHEMA_DIR = Path(__file__).resolve().parents[2] / "docs" / "protocol" / "schema"

VALID_SUMMARY = {
    "sent": 50, "confirmed": 50, "delivered": 50, "processed_unique": 50, "acked": 50, "dup_business": 0,
    "processed_rate": 1.0, "dup_rate": 0.0, "loss_rate": 0.0,
    "avg_delivery_latency_s": 0.01, "recovery_time_s": None, "verdict": "PASS", "details": [],
}
VALID_RECONCILIATION = {
    "dup_business_ids": [], "sent_not_confirmed": [], "confirmed_not_delivered": [],
    "delivered_not_processed": [], "processed_not_acked": [], "total_events": 250,
}


def make_task() -> ClaimedTask:
    return ClaimedTask(
        task_id="40", execution_id="eid-1", attempt=1,
        scenario_key="normal_delivery", params_snapshot={"message_count": 50},
    )


def outcome(**kw) -> RunnerOutcome:
    base = {"status": "COMPLETED", "exit_code": 0, "timed_out": False, "aborted": False,
                "failure": None, "results": None, "duration_s": 1.0}
    base.update(kw)
    return RunnerOutcome(**base)


def validate_result(doc) -> list:
    from jsonschema import Draft202012Validator, FormatChecker

    schema = json.loads((SCHEMA_DIR / "result.schema.json").read_text(encoding="utf-8"))
    return [e.message for e in Draft202012Validator(schema, format_checker=FormatChecker()).iter_errors(doc)]


# ---------- Task 2: assemble_result ----------


def test_completed_uses_results_and_passes_schema():
    doc = execution.assemble_result(make_task(), outcome(results={
        "summary": VALID_SUMMARY, "reconciliation": VALID_RECONCILIATION,
    }))

    assert doc["status"] == "COMPLETED" and doc["attempt"] == 1
    assert doc["summary"] == VALID_SUMMARY and doc["reconciliation"] == VALID_RECONCILIATION
    assert "failure" not in doc and doc["artifacts"] == []
    assert validate_result(doc) == []


def test_failed_without_results_synthesizes_and_passes_schema():
    doc = execution.assemble_result(make_task(), outcome(
        status="FAILED", exit_code=-9, timed_out=True,
        failure={"reason": "timeout", "message": "pytest exceeded total timeout 300s: ..."},
    ))

    assert doc["status"] == "FAILED" and doc["failure"]["reason"] == "timeout"
    assert doc["summary"]["verdict"] == "FAIL" and doc["summary"]["sent"] == 0
    assert doc["summary"]["processed_rate"] is None and doc["summary"]["details"] == []
    assert doc["reconciliation"]["total_events"] == 0
    assert validate_result(doc) == []


def test_aborted_maps_to_cancelled_without_failure():
    doc = execution.assemble_result(make_task(), outcome(status="ABORTED", exit_code=-15, aborted=True))

    assert doc["status"] == "CANCELLED" and "failure" not in doc
    assert validate_result(doc) == []


def test_failed_with_results_keeps_evidence():
    doc = execution.assemble_result(make_task(), outcome(
        status="FAILED", exit_code=1,
        failure={"reason": "error", "message": "pytest 退出码 1：..."},
        results={"summary": VALID_SUMMARY, "reconciliation": VALID_RECONCILIATION},
    ))

    assert doc["status"] == "FAILED" and doc["failure"]["reason"] == "error"
    assert doc["summary"] == VALID_SUMMARY  # 证据保留
    assert validate_result(doc) == []


def test_results_missing_required_key_raises():
    with pytest.raises(KeyError):
        execution.assemble_result(make_task(), outcome(results={"summary": VALID_SUMMARY}))


# ---------- Task 3: execute 编排（fake 注入，不发真实网络/docker 调用） ----------


class FakeControl:
    """假控制面客户端：记录事件与结果；可脚本化失败。"""

    def __init__(self):
        self.events = []  # (eid, seq, type, payload)
        self.results = []  # (eid, body)
        self.timeline = []  # 统一时间线：("event", substate) / ("result", status)——钉相对顺序
        self.event_errors = []
        self.result_errors = []

    def send_event(self, eid, seq, event_type, payload):
        if self.event_errors:
            raise self.event_errors.pop(0)
        self.events.append((eid, seq, event_type, payload))
        if event_type == "STATUS":
            self.timeline.append(("event", payload["substate"]))

    def send_result(self, eid, body):
        if self.result_errors:
            raise self.result_errors.pop(0)
        self.results.append((eid, body))
        self.timeline.append(("result", body["status"]))


class FakeNet:
    def __init__(self):
        self.connected = []
        self.disconnected = []

    def connect(self, container):
        self.connected.append(container)

    def disconnect(self, container):
        self.disconnected.append(container)


class FakeDocker:
    """假 docker client：networks.get / containers.run（setup_broker）。"""

    def __init__(self, setup_error=None):
        self.networks = FakeNetworks()
        self.setup_calls = []
        self.setup_error = setup_error

    def containers_run(self, image, command, network):
        self.setup_calls.append({"image": image, "command": command, "network": network})
        if self.setup_error:
            raise self.setup_error


class FakeNetworks:
    def __init__(self):
        self.map = {}

    def get(self, name):
        return self.map.setdefault(name, FakeNet())


class FakeEnv:
    """替身：render_lab / up / down / run_scenario 全记录、可脚本化失败。"""

    def __init__(self):
        self.outcome = outcome()
        self.up_error = None
        self.down_error = None
        self.render_calls = []
        self.up_calls = []
        self.down_calls = []
        self.run_calls = []

    def render(self, task_id, execution_id, out_dir):
        self.render_calls.append((task_id, execution_id, out_dir))
        return SimpleNamespace(task_id=task_id, execution_id=execution_id, net_name=f"brp_task_{task_id}")

    def up(self, docker_client, rendered, abort=None):
        self.up_calls.append(rendered.net_name)
        if self.up_error:
            raise self.up_error

    def down(self, docker_client, task_id, execution_id):
        self.down_calls.append((task_id, execution_id))
        if self.down_error:
            raise self.down_error

    def run(self, task, abort, **kw):
        self.run_calls.append({"task": task, "abort": abort, **kw})
        return self.outcome


@pytest.fixture
def wired(monkeypatch, tmp_path):
    """把 execution 的协作方全部换成 fake；返回操控面。"""

    class Control:
        pass

    ctl = Control()
    ctl.monkeypatch = monkeypatch  # 测试体内重新接管协作方（覆盖 fixture 期绑定）
    ctl.control = FakeControl()
    ctl.docker = FakeDocker()
    ctl.env = FakeEnv()
    ctl.sleeps = []
    ctl.work_root = tmp_path / "work"

    monkeypatch.setattr(execution, "RESULT_SEND_BACKOFF_S", (0.0, 0.0))
    monkeypatch.setattr(execution.time, "sleep", lambda s: ctl.sleeps.append(s))
    monkeypatch.setattr(execution.dc, "get_client", lambda: ctl.docker)
    monkeypatch.setattr(execution.lab_factory, "render_lab", ctl.env.render)
    monkeypatch.setattr(execution.lab_factory, "up", ctl.env.up)
    monkeypatch.setattr(execution.lab_factory, "down", ctl.env.down)
    monkeypatch.setattr(execution, "_setup_broker", lambda docker, rendered, task_id: ctl.docker.containers_run(
        "brp-lab:dev", ["python", "setup"], rendered.net_name))
    monkeypatch.setattr(execution, "run_scenario", ctl.env.run)
    return ctl


def run_execute(ctl, task=None, abort=None):
    execution.execute(
        task or make_task(),
        abort or threading.Event(),
        client=ctl.control,
        scenarios_root=Path("/scen"),
        work_root=ctl.work_root,
        agent_container="brp-agent",
        total_timeout_s=300,
    )


def substates_of(control):
    return [payload["substate"] for _, _, _, payload in control.events]


def test_happy_path(wired):
    ctl = wired

    run_execute(ctl)

    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "BASELINE", "UPLOADING", "DONE"]
    seqs = [seq for _, seq, _, _ in ctl.control.events]
    assert seqs == list(range(len(seqs)))  # 从 0 起严格递增
    ((eid, body),) = ctl.control.results
    assert eid == "eid-1" and body["status"] == "COMPLETED"
    assert ctl.env.render_calls[0][:2] == ("40", "eid-1")
    assert ctl.env.up_calls == ["brp_task_40"]
    assert ctl.env.down_calls == [("40", "eid-1")]
    assert ctl.env.run_calls[0]["task"].task_id == "40"
    assert ctl.docker.networks.map["brp_task_40"].connected == ["brp-agent"]
    assert ctl.docker.networks.map["brp_task_40"].disconnected == ["brp-agent"]
    # 相对顺序：DONE 事件在 result 之前（Go 对终态后事件只记录不广播——反序前端看不到 DONE）
    timeline = ctl.control.timeline
    assert timeline.index(("event", "DONE")) < timeline.index(("result", "COMPLETED"))


def test_env_up_failure_reports_error_and_still_cleans(wired):
    ctl = wired
    ctl.env.up_error = RuntimeError("broker not healthy")

    run_execute(ctl)

    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING"]
    ((_, body),) = ctl.control.results
    assert body["status"] == "FAILED" and body["failure"]["reason"] == "error"
    assert "broker not healthy" in body["failure"]["message"]
    assert ctl.env.down_calls == [("40", "eid-1")]


def test_setup_broker_failure_reports_error(wired):
    ctl = wired
    ctl.docker.setup_error = RuntimeError("exit 1: channel error")

    run_execute(ctl)

    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING"]
    ((_, body),) = ctl.control.results
    assert body["status"] == "FAILED" and "exit 1" in body["failure"]["message"]


def test_runner_timeout_maps_through(wired):
    ctl = wired
    ctl.env.outcome = outcome(
        status="FAILED", exit_code=-9, timed_out=True,
        failure={"reason": "timeout", "message": "pytest exceeded total timeout 300s: ..."},
    )

    run_execute(ctl)

    ((_, body),) = ctl.control.results
    assert body["failure"]["reason"] == "timeout"
    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "BASELINE", "UPLOADING"]


def test_abort_maps_to_cancelled(wired):
    ctl = wired
    ctl.env.outcome = outcome(status="ABORTED", exit_code=-15, aborted=True)

    run_execute(ctl)

    ((_, body),) = ctl.control.results
    assert body["status"] == "CANCELLED" and "failure" not in body


def test_result_send_retries_then_succeeds(wired):
    ctl = wired
    ctl.control.result_errors = [ControlPlaneError("连不上", None)] * 2

    run_execute(ctl)

    assert len(ctl.control.results) == 1  # 第三次成功
    assert len(ctl.sleeps) == 2


def test_result_send_gives_up_after_three_attempts(wired):
    ctl = wired
    ctl.control.result_errors = [ControlPlaneError("连不上", None)] * 9

    run_execute(ctl)  # 不抛：放弃后由租约清扫收口（记日志）

    assert len(ctl.sleeps) == 2 and ctl.control.results == []


def test_teardown_failure_does_not_block_result(wired):
    ctl = wired
    ctl.env.down_error = RuntimeError("network has active endpoints")

    run_execute(ctl)

    ((_, body),) = ctl.control.results
    assert body["status"] == "COMPLETED"


def test_event_transient_failure_retried_same_seq(wired):
    """瞬时失败同 seq 重试一次（计划 Global Constraints；评审 I-2）——重试成功则事件不丢。"""
    ctl = wired
    ctl.control.event_errors = [ControlPlaneError("5xx", 502)]

    run_execute(ctl)

    assert len(ctl.control.events) == 5  # 一次瞬时失败被重试吸收
    seqs = [seq for _, seq, _, _ in ctl.control.events]
    assert seqs == list(range(5))  # 重试用同一 seq（不是跳号）


def test_event_failure_after_retry_tolerated(wired):
    ctl = wired
    ctl.control.event_errors = [ControlPlaneError("5xx", 502), ControlPlaneError("连不上", None)]

    run_execute(ctl)

    assert len(ctl.control.events) == 4  # 两次都失败才放弃（记日志，流程不中断）
    assert ctl.control.results[0][1]["status"] == "COMPLETED"


def test_assemble_failure_falls_back_to_error(wired):
    ctl = wired
    ctl.env.outcome = outcome(results={"summary": VALID_SUMMARY})  # 缺 reconciliation

    run_execute(ctl)

    ((_, body),) = ctl.control.results
    assert body["status"] == "FAILED" and body["failure"]["reason"] == "error"
    assert body["summary"]["verdict"] == "FAIL"  # 合成兜底


# ---------- Task 2（day41）：取消路径——环境段检查点 + ABORTED 事件先于 result ----------


def test_abort_before_env_skips_to_cancelled(wired):
    """领取后立即取消：不发 BASELINE、不碰环境、清理照走、ABORTED 事件先于 result。"""
    ctl = wired
    abort = threading.Event()
    abort.set()

    run_execute(ctl, abort=abort)

    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "ABORTED"]
    assert ctl.env.up_calls == []  # 环境未拉起
    assert ctl.env.down_calls == []  # 未拉起无资源可清（_teardown 对 None 直接返回）
    ((_, body),) = ctl.control.results
    assert body["status"] == "CANCELLED" and "failure" not in body
    timeline = ctl.control.timeline
    assert timeline.index(("event", "ABORTED")) < timeline.index(("result", "CANCELLED"))
    assert ctl.env.run_calls == []  # runner 未起


def test_abort_during_env_phase_caught_at_checkpoint(wired):
    """环境拉起中途取消：up 之后的检查点拦住，不再 connect/setup_broker/起 runner。"""
    ctl = wired
    abort = threading.Event()
    original_up = ctl.env.up

    def up_then_abort(docker_client, rendered, **kw):
        original_up(docker_client, rendered)
        abort.set()  # 外层 Event（kw 里的 abort=is_set 函数，不遮蔽）

    # fixture 的 monkeypatch 绑定的是原 bound method——测试里须重新接管才生效
    ctl.monkeypatch.setattr(execution.lab_factory, "up", up_then_abort)

    run_execute(ctl, abort=abort)

    assert ctl.env.up_calls == ["brp_task_40"]
    assert all(net.connected == [] for net in ctl.docker.networks.map.values())  # connect 未发生
    assert ctl.docker.setup_calls == []
    assert ctl.env.run_calls == []
    ((_, body),) = ctl.control.results
    assert body["status"] == "CANCELLED"
    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "ABORTED"]


def test_up_cancelled_converges_to_cancelled(wired):
    """up 健康等待期取消（LabCancelled，评审 M-2）：收敛为 result(CANCELLED) 而非 FAILED。"""
    ctl = wired
    from envmgr.lab_factory import LabCancelled

    def up_cancelled(docker_client, rendered, **kw):
        ctl.env.up_calls.append(rendered.net_name)
        raise LabCancelled("container brp_40_broker health wait cancelled")

    ctl.monkeypatch.setattr(execution.lab_factory, "up", up_cancelled)

    run_execute(ctl)

    ((_, body),) = ctl.control.results
    assert body["status"] == "CANCELLED" and "failure" not in body
    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "ABORTED"]
    assert ctl.env.down_calls == [("40", "eid-1")]
    assert ctl.env.run_calls == []


def test_runner_abort_emits_aborted_event_too(wired):
    """runner 段取消（day39 outcome）：同样先发 ABORTED 再回传 result(CANCELLED)。"""
    ctl = wired
    ctl.env.outcome = outcome(status="ABORTED", exit_code=-15, aborted=True)

    run_execute(ctl)

    timeline = ctl.control.timeline
    assert timeline.index(("event", "ABORTED")) < timeline.index(("result", "CANCELLED"))
    assert substates_of(ctl.control) == ["CLAIMED", "PREPARING", "BASELINE", "UPLOADING", "ABORTED"]
