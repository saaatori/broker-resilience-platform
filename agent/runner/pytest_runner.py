"""runner/pytest_runner.py（M2 第 39 天）：params.json 落盘 → pytest 子进程 → 总超时强杀 → 读 results.json。

子进程命令（实施规划 §6 / lab/conftest 用法口径）：
    python -m pytest <scenarios_root>/<scenario_key> --params-path <work>/params.json -p no:cacheprovider

停止协议（超时与取消同一条路径）：SIGTERM → 宽限 graceful_s（默认 5s）→ SIGKILL；
停止原因一次性定格（先触发者为准，后到的事件不改判）。POSIX 下子进程独立会话
（start_new_session），信号发往进程组——pytest 自己起的子进程一并收口；
Windows 无信号语义，terminate()/kill() 同为硬终止，消息如实写 "terminated"。
stdout/stderr 重定向到工作目录 pytest.log（文件流，不接管道——写满会阻塞子进程，排障 #36）。
results.json 契约：场景（其插件）在子进程 CWD（=工作目录）写 results.json，runner 收尾读回。
边界（第 40 天）：结果体组装（attempt 注入 / summary 兜底 / artifacts / 上传）、实验环境拉起、
agent 镜像打包（lab/ 进镜像）都不在本模块；abort 为鸭子类型
（executor.AbortSignal / threading.Event 同形：wait(timeout) -> bool）。
"""

from __future__ import annotations

import contextlib
import json
import os
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from executor.client import ClaimedTask

TOTAL_TIMEOUT_S = 300.0  # 总超时默认（协议 §2.5.1 样例口径 "total timeout 300s"）
GRACEFUL_S = 5.0  # SIGTERM→SIGKILL 宽限（实施规划 §6，固定口径）
POLL_S = 0.05  # 超时/取消判定的唤醒粒度
WORK_ROOT = Path("/work")  # 生产工作目录根（compose 卷，day 40 接线）
REPO_ROOT = Path(__file__).resolve().parents[2]  # 子进程 PYTHONPATH 基准（lab.*/agent.* 可导入）


@dataclass
class RunnerOutcome:
    status: str  # "COMPLETED" | "FAILED" | "ABORTED"
    exit_code: int | None
    timed_out: bool
    aborted: bool
    failure: dict | None  # 协议 §2.5.1 {reason, message}（FAILED 时给出；ABORTED 不带——schema 限 FAILED）
    results: dict | None  # results.json 内容（读到即给，作为证据）
    duration_s: float


def write_params_file(work_dir: Path, task: ClaimedTask) -> Path:
    """params.json 落盘（协议 §2.3 合成式）：snapshot + 顶层 task_id/execution_id。

    注入键覆盖同名快照键——task_id/execution_id 以 claim 响应顶层为准（Go day31 钉子同口径）。
    """
    work_dir = Path(work_dir)
    work_dir.mkdir(parents=True, exist_ok=True)
    path = work_dir / "params.json"
    payload = {**task.params_snapshot, "task_id": task.task_id, "execution_id": task.execution_id}
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


# ---------- 停止信号（平台缝；单测经 monkeypatch 接管） ----------


def _send_terminate(proc) -> None:
    """POSIX：SIGTERM 发往子进程组（start_new_session 下 pgid==pid，pytest 的子进程一并收口）；
    Windows：terminate() 即 TerminateProcess 硬终止（无信号语义）。"""
    if os.name == "posix":
        with contextlib.suppress(ProcessLookupError):
            os.killpg(proc.pid, signal.SIGTERM)
    else:
        proc.terminate()


def _send_kill(proc) -> None:
    if os.name == "posix":
        with contextlib.suppress(ProcessLookupError):
            os.killpg(proc.pid, signal.SIGKILL)
    else:
        proc.kill()


def _timeout_message(total_s: float, graceful_s: float, exit_code: int | None, killed: bool) -> str:
    """§2.5.1 样例格式；按实际发生的事件如实陈述。分支依据是证据（退出码/是否升级过），
    不是宿主平台——Windows 真实路径的退出码是 1（硬终止），自然落入 terminated 分支。"""
    total, graceful = f"{total_s:g}", f"{graceful_s:g}"
    # 注：Windows 下 terminate() 即时致死，升级分支（killed=True）在 Windows 不可达——
    # 下面 SIGTERM/SIGKILL 句式仅 POSIX 升级路径会产出（评审 M-6 记档）
    if killed or exit_code == -9:  # SIGKILL（POSIX 编号）
        return f"pytest exceeded total timeout {total}s: SIGTERM sent, SIGKILL after {graceful}s (exit={exit_code})"
    if exit_code == -15:  # SIGTERM（POSIX 编号）
        return f"pytest exceeded total timeout {total}s: SIGTERM sent (exit={exit_code})"
    return f"pytest exceeded total timeout {total}s: terminated (hard-kill, exit={exit_code})"


def _read_results(work_dir: Path) -> tuple[dict | None, str | None]:
    """读 results.json；返回 (content, error)——二者恰一为 None。"""
    path = work_dir / "results.json"
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None, "文件不存在"
    except (OSError, ValueError) as e:
        return None, f"无法解析（{e}）"
    if not isinstance(data, dict):
        return None, f"顶层不是 JSON 对象（{type(data).__name__}）"
    return data, None


def _log_tail(path: Path, limit: int = 500) -> str:
    try:
        return path.read_text(encoding="utf-8", errors="replace")[-limit:].strip()
    except OSError:
        return "<pytest.log 不可读>"


def run_scenario(
    task: ClaimedTask,
    abort,
    *,
    scenarios_root: Path,
    work_root: Path = WORK_ROOT,
    total_timeout_s: float = TOTAL_TIMEOUT_S,
    graceful_s: float = GRACEFUL_S,
) -> RunnerOutcome:
    """跑一个执行：落盘 → pytest 子进程 → 停止协议（超时/取消同路径）→ 读 results.json。

    阻塞至子进程收口（正常退出或被杀）；停止原因一次性定格——先触发者为准。
    work_root / scenarios_root 相对或绝对均可（入口统一转绝对：子进程 cwd 变更后
    相对路径会在子进程侧二次解析而错位）。
    """
    work_dir = (Path(work_root) / task.execution_id).resolve()
    params_path = write_params_file(work_dir, task)
    scenario_dir = (Path(scenarios_root) / task.scenario_key).resolve()
    cmd = [
        sys.executable,
        "-m",
        "pytest",
        str(scenario_dir),
        "--params-path",
        str(params_path),
        "-p",
        "no:cacheprovider",
    ]
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        p for p in (str(REPO_ROOT), env.get("PYTHONPATH", "")) if p
    )

    timed_out = False
    aborted = False
    killed = False
    started = time.monotonic()
    with open(work_dir / "pytest.log", "w", encoding="utf-8") as log:
        popen_kwargs: dict[str, Any] = {
            "cwd": str(work_dir),
            "env": env,
            "stdout": log,  # 输出落文件不接管道：PIPE 不读会写满阻塞子进程（排障 #36）
            "stderr": subprocess.STDOUT,
        }
        if os.name == "posix":
            popen_kwargs["start_new_session"] = True  # 独立进程组：killpg 不会误伤 Agent 自己
        proc = subprocess.Popen(cmd, **popen_kwargs)
        deadline = started + total_timeout_s
        stopping_since: float | None = None
        while proc.poll() is None:
            if stopping_since is None:
                if abort.wait(timeout=min(POLL_S, max(deadline - time.monotonic(), 0.0))):
                    aborted = True  # 停止原因定格：进入停止窗口后，后到的事件不再改判
                    _send_terminate(proc)
                    stopping_since = time.monotonic()
                elif time.monotonic() >= deadline:
                    timed_out = True
                    _send_terminate(proc)
                    stopping_since = time.monotonic()
            elif time.monotonic() - stopping_since >= graceful_s:
                _send_kill(proc)
                killed = True
                proc.wait()  # SIGKILL 后必返；防御性无超时等待收尸（防僵尸）
                break
            else:
                abort.wait(timeout=POLL_S)  # 停止窗口内的短睡（复用 Event 计时）
        exit_code = proc.returncode
    duration_s = time.monotonic() - started

    results, results_error = _read_results(work_dir)
    if aborted:
        status, failure = "ABORTED", None
    elif timed_out:
        status = "FAILED"
        failure = {
            "reason": "timeout",
            "message": _timeout_message(total_timeout_s, graceful_s, exit_code, killed),
        }
    elif exit_code == 0:
        if results is None:
            status = "FAILED"
            failure = {"reason": "error", "message": f"pytest 正常结束但 results.json 不可用：{results_error}"}
        else:
            status, failure = "COMPLETED", None
    else:
        status = "FAILED"
        failure = {"reason": "error", "message": f"pytest 退出码 {exit_code}：{_log_tail(work_dir / 'pytest.log')}"}
    return RunnerOutcome(status, exit_code, timed_out, aborted, failure, results, duration_s)
