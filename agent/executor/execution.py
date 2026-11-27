"""executor/execution.py（M2 第 40 天）：execute 编排——环境生命周期 + 事件序列 + runner + 回传。

把 day38 的执行注入位、day39 的 runner、实验环境工厂与协议回传拼成端到端执行流：
    STATUS(CLAIMED) → PREPARING(环境拉起) → BASELINE(setup 完成，起 pytest)
    → UPLOADING(runner 返回) → [清理] → result 回传 → STATUS(DONE)
失败/取消路径收敛为同一收尾：清理（disconnect + label 白名单 down）→ result 回传。
回传在 execute 返回前完成（day38 契约：返回即出在跑表、心跳停续租）；
result 落库后 Go 将执行置 DONE，此后心跳差集会给出 lease_lost 中止——按 day38 M7 记档当空操作。
"""

from __future__ import annotations

import itertools
import logging
import time
from pathlib import Path

from envmgr import docker_client as dc
from envmgr import lab_factory
from executor.client import ClaimedTask, ControlPlaneError
from runner.pytest_runner import (
    TOTAL_TIMEOUT_S,
    WORK_ROOT,
    RunnerOutcome,
    run_scenario,
)

RESULT_SEND_ATTEMPTS = 3
RESULT_SEND_BACKOFF_S = (1.0, 3.0)

SETUP_BROKER_IMAGE = "brp-lab:dev"

REPO_ROOT = Path(__file__).resolve().parents[2]
SCENARIOS_ROOT = REPO_ROOT / "lab" / "scenarios"

log = logging.getLogger("brp-agent")

# 合成兜底 summary（失败/取消且无 results.json）：schema 必填键全给，计数 0、指标 null、verdict FAIL
_SYNTH_SUMMARY = {
    "sent": 0, "confirmed": 0, "delivered": 0, "processed_unique": 0, "acked": 0, "dup_business": 0,
    "processed_rate": None, "dup_rate": None, "loss_rate": None,
    "avg_delivery_latency_s": None, "recovery_time_s": None,
    "verdict": "FAIL", "details": [],
}
_SYNTH_RECONCILIATION = {
    "dup_business_ids": [], "sent_not_confirmed": [], "confirmed_not_delivered": [],
    "delivered_not_processed": [], "processed_not_acked": [], "total_events": 0,
}

_STATUS_BY_OUTCOME = {"COMPLETED": "COMPLETED", "FAILED": "FAILED", "ABORTED": "CANCELLED"}


def assemble_result(task: ClaimedTask, outcome: RunnerOutcome) -> dict:
    """runner 事实性 outcome → 协议 §2.5 result 请求体（纯函数）。

    有 results.json（场景产出）则用之（缺 summary/reconciliation 键=场景契约违约，抛 KeyError
    交由 execute 转 FAILED/error）；无则合成兜底（计数 0、指标 null、verdict FAIL）。
    failure 仅 FAILED 携带（§2.5.1；ABORTED→CANCELLED 不带）。
    """
    if outcome.results is not None:
        summary = outcome.results["summary"]
        reconciliation = outcome.results["reconciliation"]
    else:
        summary, reconciliation = _SYNTH_SUMMARY, _SYNTH_RECONCILIATION
    doc = {
        "attempt": task.attempt,
        "status": _STATUS_BY_OUTCOME[outcome.status],
        "summary": summary,
        "reconciliation": reconciliation,
        "artifacts": [],
    }
    if outcome.status == "FAILED":
        doc["failure"] = outcome.failure or {"reason": "error", "message": "执行失败（无 failure 说明）"}
    return doc


def execute(
    task: ClaimedTask,
    abort,
    *,
    client,
    scenarios_root: Path,
    work_root: Path = WORK_ROOT,
    agent_container: str = "brp-agent",
    total_timeout_s: float = TOTAL_TIMEOUT_S,
) -> None:
    """执行一个领取到的任务（day38 Execute 注入位的实现，全程在执行线程内）。

    环境段任何异常 → FAILED(error) 合成结果；runner 的 outcome 原样映射；
    清理（disconnect + label 白名单 down）先于回传；回传重试 3 次，
    放弃后由租约清扫收口（如实失败模式）。DONE 事件在 result 之前、且仅正常路径发
    （§2.5 收口归属：终态子状态先行不影响 result 收口；反序则 Go 只记录不广播）。
    """
    seq = itertools.count()

    def status(substate: str) -> None:
        n = next(seq)
        for attempt in (1, 2):  # 瞬时失败同 seq 重试一次（幂等键 (eid, seq) 保证安全）
            try:
                client.send_event(task.execution_id, n, "STATUS", {"substate": substate})
                return
            except ControlPlaneError as e:
                if attempt == 2:
                    # 仍失败才放弃：后续 STATUS 同样触发 Go 侧转移（首条丢了也不卡状态机）
                    log.warning("事件上报失败（已重试一次，跳过）：task=%s exec=%s substate=%s：%s",
                                task.task_id, task.execution_id, substate, e)

    docker = None
    rendered = None
    try:
        status("CLAIMED")
        status("PREPARING")
        docker = dc.get_client()
        rendered = lab_factory.render_lab(
            task.task_id, task.execution_id, Path(work_root) / task.execution_id / "lab"
        )
        lab_factory.up(docker, rendered)
        docker.networks.get(rendered.net_name).connect(agent_container)
        _setup_broker(docker, rendered, task.task_id)
        status("BASELINE")
        runner_outcome = run_scenario(
            task, abort, scenarios_root=Path(scenarios_root),
            work_root=work_root, total_timeout_s=total_timeout_s,
        )
        status("UPLOADING")
    except Exception as e:
        log.exception("环境/执行阶段失败：task=%s exec=%s", task.task_id, task.execution_id)
        runner_outcome = RunnerOutcome(
            status="FAILED", exit_code=None, timed_out=False, aborted=False,
            failure={"reason": "error", "message": f"{type(e).__name__}: {e}"[:1024]},
            results=None, duration_s=0.0,
        )
    finally:
        _teardown(docker, rendered, agent_container)

    try:
        doc = assemble_result(task, runner_outcome)
    except Exception as e:
        log.exception("结果体组装失败（场景 results.json 契约违约，改判 FAILED/error）")
        runner_outcome = RunnerOutcome(
            status="FAILED", exit_code=None, timed_out=False, aborted=False,
            failure={"reason": "error", "message": f"结果体组装失败：{type(e).__name__}: {e}"[:1024]},
            results=None, duration_s=0.0,
        )
        doc = assemble_result(task, runner_outcome)
    # DONE 先于 result（§2.5 收口归属：先经 STATUS 上报终态子状态不影响 result 收口；
    # 反序则 DONE 落在任务终态之后——Go 侧只记录不广播（events.go 终态兜底），前端看不到 DONE）
    if runner_outcome.status == "COMPLETED":
        status("DONE")
    _send_result(client, task, doc)
    log.info("执行收尾：task=%s exec=%s status=%s", task.task_id, task.execution_id, doc["status"])


def _setup_broker(docker, rendered, task_id: str) -> None:
    """任务网内一次性 brp-lab 容器跑 RabbitMQ 标准配置（镜像内烘焙代码；非零退出抛 ContainerError）。"""
    docker.containers.run(
        SETUP_BROKER_IMAGE,
        ["python", "/app/lab/helpers/setup_broker.py", "--host", f"brp_{task_id}_broker"],
        network=rendered.net_name,
        remove=True,
    )


def _teardown(docker, rendered, agent_container: str) -> None:
    """disconnect 本容器 → label 白名单 down；两步失败都只记日志（不阻断回传）。"""
    if docker is None or rendered is None:
        return
    try:
        docker.networks.get(rendered.net_name).disconnect(agent_container)
    except Exception as e:  # noqa: BLE001 — 清理路径只记日志（down 仍会强制移除网络）
        log.warning("断开实验网络失败（忽略）：task=%s：%s", rendered.task_id, e)
    try:
        lab_factory.down(docker, rendered.task_id, rendered.execution_id)
    except Exception as e:  # noqa: BLE001 — 残留由 Go 清理任务队列兜底（day43 消费）
        log.warning("实验环境清理失败（残留由 Go 清理任务兜底）：task=%s：%s", rendered.task_id, e)


def _send_result(client, task: ClaimedTask, doc: dict) -> None:
    """result 回传：重试 3 次（退避 1s/3s），放弃则记日志——任务由租约清扫收口。"""
    for attempt in range(1, RESULT_SEND_ATTEMPTS + 1):
        try:
            client.send_result(task.execution_id, doc)
            return
        except ControlPlaneError as e:
            if attempt == RESULT_SEND_ATTEMPTS:
                log.error("结果回传放弃（%d 次尝试，任务将由租约清扫收口）：task=%s exec=%s：%s",
                          attempt, task.task_id, task.execution_id, e)
                return
            delay = RESULT_SEND_BACKOFF_S[attempt - 1]
            log.warning("结果回传失败（%.0fs 后重试 %d/%d）：task=%s exec=%s：%s",
                        delay, attempt + 1, RESULT_SEND_ATTEMPTS, task.task_id, task.execution_id, e)
            time.sleep(delay)
