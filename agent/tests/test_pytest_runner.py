"""runner/pytest_runner 单测（第 39 天）：合成式落盘 + 子进程停止状态机。

FakeProcess 只模拟 runner 用到的 Popen 面（poll/wait/returncode），信号语义经
monkeypatch 接管 _send_terminate/_send_kill——平台真实行为由真实子进程用例与
容器验收（POSIX 真信号）覆盖。
"""

import io
import json
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

import agent.runner.pytest_runner as runner
import pytest
from agent.runner.pytest_runner import write_params_file
from jsonschema import Draft202012Validator, FormatChecker

from executor.client import ClaimedTask

SCHEMA_DIR = Path(__file__).resolve().parents[2] / "docs" / "protocol" / "schema"

FAST = {"total_timeout_s": 0.3, "graceful_s": 0.2}


def make_task(**snapshot) -> ClaimedTask:
    params = {"message_count": 50, "idle_timeout_s": 30, "assertions": []}
    params.update(snapshot)
    return ClaimedTask(
        task_id="7", execution_id="eid-1", attempt=1, scenario_key="demo", params_snapshot=params
    )


# ---------- Task 1: params.json 合成式落盘 ----------


def test_write_params_file_composes_snapshot_plus_ids(tmp_path):
    task = make_task()

    data = json.loads(write_params_file(tmp_path / "w", task).read_text(encoding="utf-8"))

    assert data == {**task.params_snapshot, "task_id": "7", "execution_id": "eid-1"}


def test_write_params_file_injected_keys_win(tmp_path):
    task = make_task(task_id="oops")  # snapshot 里混入同名键：注入值必须覆盖

    data = json.loads(write_params_file(tmp_path, task).read_text(encoding="utf-8"))

    assert data["task_id"] == "7"


def test_write_params_file_overwrites(tmp_path):
    write_params_file(tmp_path, make_task())

    write_params_file(tmp_path, make_task(message_count=99))

    data = json.loads((tmp_path / "params.json").read_text(encoding="utf-8"))
    assert data["message_count"] == 99 and data["task_id"] == "7"


def test_write_params_file_output_passes_protocol_schema(tmp_path):
    """产物过 params_snapshot schema——把 day31 的合成式钉子从手写 dict 换到真函数产物。"""
    sample = json.loads((SCHEMA_DIR / "samples" / "params.sample.json").read_text(encoding="utf-8"))
    snapshot = {k: v for k, v in sample.items() if k not in ("task_id", "execution_id")}
    task = ClaimedTask(
        task_id="42",
        execution_id="0f0e0d0c-baad-1dea-dead-beef00000042",
        attempt=1,
        scenario_key="normal_delivery",
        params_snapshot=snapshot,
    )

    data = json.loads(write_params_file(tmp_path, task).read_text(encoding="utf-8"))

    schema = json.loads((SCHEMA_DIR / "params_snapshot.schema.json").read_text(encoding="utf-8"))
    errors = [e.message for e in Draft202012Validator(schema, format_checker=FormatChecker()).iter_errors(data)]
    assert errors == [], errors


# ---------- Task 2: 停止状态机（FakeProcess） ----------


class FakeProcess:
    """只模拟 runner 用到的 Popen 面；信号效果由 fake 的 _send_* 注入。"""

    def __init__(self, returncode=None, ignore_sigterm=False):
        self.pid = 4242
        self.returncode = returncode
        self.ignore_sigterm = ignore_sigterm
        self.signals = []
        self.cmd = None
        self.kwargs = None

    def poll(self):
        return self.returncode

    def wait(self, timeout=None):
        return self.returncode


@pytest.fixture
def fake_popen(monkeypatch):
    """接管 Popen 与两个信号函数；返回 proc 供断言。用法：proc = fake_popen(FakeProcess(...))。"""

    def factory(proc: FakeProcess) -> FakeProcess:
        def popen(cmd, **kwargs):
            proc.cmd = cmd
            proc.kwargs = kwargs
            return proc

        def term(p):
            p.signals.append("TERM")
            if not p.ignore_sigterm:
                p.returncode = -15

        def kill(p):
            p.signals.append("KILL")
            p.returncode = -9

        monkeypatch.setattr(runner.subprocess, "Popen", popen)
        monkeypatch.setattr(runner, "_send_terminate", term)
        monkeypatch.setattr(runner, "_send_kill", kill)
        return proc

    return factory


def preset_results(work_root: Path, eid: str = "eid-1", content: str = '{"summary": {"verdict": "PASS"}, "k": 1}'):
    work = work_root / eid
    work.mkdir(parents=True, exist_ok=True)
    (work / "results.json").write_text(content, encoding="utf-8")
    return work


def test_invocation_shape(tmp_path, fake_popen):
    proc = fake_popen(FakeProcess(returncode=0))
    preset_results(tmp_path)

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path / "scen", work_root=tmp_path, **FAST
    )

    assert outcome.status == "COMPLETED"  # 顺带：开局即死 → 正常分类
    assert proc.signals == []  # 不补刀
    assert proc.cmd[0] == sys.executable
    assert proc.cmd[1:4] == ["-m", "pytest", str(tmp_path / "scen" / "demo")]
    assert proc.cmd[4:6] == ["--params-path", str(tmp_path / "eid-1" / "params.json")]
    assert proc.cmd[6:] == ["-p", "no:cacheprovider"]
    assert proc.kwargs["cwd"] == str(tmp_path / "eid-1")
    assert proc.kwargs["env"]["PYTHONPATH"].split(os.pathsep)[0] == str(runner.REPO_ROOT)
    assert (tmp_path / "eid-1" / "params.json").exists()
    # 输出必须是文件流、不能是 PIPE——管道不读会写满阻塞子进程（排障 #36，评审 I-2）
    assert isinstance(proc.kwargs["stdout"], io.IOBase)
    assert proc.kwargs["stderr"] == subprocess.STDOUT
    # 进程组隔离只在 POSIX 存在（Windows 传该参数 Popen 直接 ValueError，评审 M-3）
    if os.name == "posix":
        assert proc.kwargs["start_new_session"] is True
    else:
        assert "start_new_session" not in proc.kwargs


def test_normal_completion_reads_results(tmp_path, fake_popen):
    fake_popen(FakeProcess(returncode=0))
    preset_results(tmp_path)

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert outcome.status == "COMPLETED"
    assert outcome.results == {"summary": {"verdict": "PASS"}, "k": 1}
    assert outcome.failure is None and not outcome.timed_out and not outcome.aborted


def test_timeout_sigterm_suffices(tmp_path, fake_popen):
    proc = fake_popen(FakeProcess(returncode=None))  # 存活到收到 TERM

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert proc.signals == ["TERM"]
    assert outcome.status == "FAILED" and outcome.timed_out
    assert outcome.failure["reason"] == "timeout"
    assert "total timeout 0.3s" in outcome.failure["message"]
    assert "SIGTERM sent (exit=-15)" in outcome.failure["message"]


def test_timeout_escalates_to_sigkill(tmp_path, fake_popen):
    proc = fake_popen(FakeProcess(returncode=None, ignore_sigterm=True))

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert proc.signals == ["TERM", "KILL"]
    assert outcome.exit_code == -9
    assert outcome.failure["reason"] == "timeout"
    assert "SIGKILL after 0.2s (exit=-9)" in outcome.failure["message"]


def test_abort_classifies_aborted(tmp_path, fake_popen):
    proc = fake_popen(FakeProcess(returncode=None))
    abort = threading.Event()
    abort.set()

    outcome = runner.run_scenario(
        make_task(), abort, scenarios_root=tmp_path, work_root=tmp_path, total_timeout_s=30, graceful_s=0.2
    )

    assert proc.signals == ["TERM"]
    assert outcome.status == "ABORTED" and outcome.aborted and not outcome.timed_out
    assert outcome.failure is None


def test_abort_with_ignored_sigterm_escalates_but_stays_aborted(tmp_path, fake_popen):
    proc = fake_popen(FakeProcess(returncode=None, ignore_sigterm=True))
    abort = threading.Event()
    abort.set()

    outcome = runner.run_scenario(
        make_task(), abort, scenarios_root=tmp_path, work_root=tmp_path, total_timeout_s=30, graceful_s=0.2
    )

    assert proc.signals == ["TERM", "KILL"]
    assert outcome.status == "ABORTED"


def test_abort_during_graceful_window_does_not_reclassify(tmp_path, fake_popen):
    """先超时进入停止窗口、宽限中取消置位：停止原因已定格为 timeout。

    独立时序（裕量 150ms×2，评审 M-1）：TERM@0.5 → abort@0.65 → KILL@0.9。
    """
    fake_popen(FakeProcess(returncode=None, ignore_sigterm=True))
    abort = threading.Event()
    threading.Timer(0.65, abort.set).start()

    outcome = runner.run_scenario(
        make_task(), abort, scenarios_root=tmp_path, work_root=tmp_path,
        total_timeout_s=0.5, graceful_s=0.4,
    )

    assert outcome.timed_out and not outcome.aborted
    assert outcome.status == "FAILED" and outcome.failure["reason"] == "timeout"


def test_zero_exit_without_results_is_error(tmp_path, fake_popen):
    fake_popen(FakeProcess(returncode=0))
    (tmp_path / "eid-1").mkdir(parents=True)  # 无 results.json

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert outcome.status == "FAILED"
    assert outcome.failure["reason"] == "error"
    assert "results.json" in outcome.failure["message"]


def test_nonzero_exit_keeps_results_evidence(tmp_path, fake_popen):
    fake_popen(FakeProcess(returncode=1))
    preset_results(tmp_path)

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert outcome.status == "FAILED"
    assert outcome.failure["reason"] == "error"
    assert "退出码 1" in outcome.failure["message"]
    assert outcome.results == {"summary": {"verdict": "PASS"}, "k": 1}  # 证据保留


def test_corrupt_results_json_is_error(tmp_path, fake_popen):
    fake_popen(FakeProcess(returncode=0))
    preset_results(tmp_path, content="not-json{")

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert outcome.status == "FAILED"
    assert outcome.failure["reason"] == "error"
    assert "results.json" in outcome.failure["message"]


def test_results_json_non_object_is_error(tmp_path, fake_popen):
    fake_popen(FakeProcess(returncode=0))
    preset_results(tmp_path, content="[1, 2]")

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=tmp_path, work_root=tmp_path, **FAST
    )

    assert outcome.status == "FAILED"
    assert outcome.failure["reason"] == "error"


# ---------- Task 3: 真实子进程（host 可跑；Windows 下 terminate 为硬终止，断言取平台无关部分） ----------

MINI_CONFTEST = '''\
def pytest_addoption(parser):
    parser.addoption("--params-path", default="params.json")
'''


def make_scenario(tmp_path, test_body):
    """临时场景目录 + 定义 --params-path 的 mini conftest（runner 固定传该参数）。"""
    scen = tmp_path / "scen" / "demo"
    scen.mkdir(parents=True)
    (scen / "conftest.py").write_text(MINI_CONFTEST, encoding="utf-8")
    (scen / "test_flow.py").write_text(test_body, encoding="utf-8")
    return tmp_path / "scen"


def test_real_subprocess_normal_path(tmp_path):
    scen = make_scenario(
        tmp_path,
        "import json\nfrom pathlib import Path\n\n"
        "def test_ok():\n"
        '    Path("results.json").write_text(json.dumps({"summary": {"verdict": "PASS"}, "marker": "day39"}), encoding="utf-8")\n',
    )

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=scen,
        work_root=tmp_path / "work", total_timeout_s=60, graceful_s=2,
    )

    assert outcome.status == "COMPLETED", outcome
    assert outcome.results == {"summary": {"verdict": "PASS"}, "marker": "day39"}
    assert (tmp_path / "work" / "eid-1" / "pytest.log").exists()
    assert len((tmp_path / "work" / "eid-1" / "pytest.log").read_text(encoding="utf-8")) > 0
    assert (tmp_path / "work" / "eid-1" / "params.json").exists()


def test_real_subprocess_timeout_is_killed_and_classified(tmp_path):
    scen = make_scenario(tmp_path, "import time\n\ndef test_sleep():\n    time.sleep(30)\n")
    started = time.monotonic()

    outcome = runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=scen,
        work_root=tmp_path / "work", total_timeout_s=1.0, graceful_s=1.0,
    )

    assert outcome.status == "FAILED" and outcome.timed_out
    assert outcome.failure["reason"] == "timeout"
    assert "total timeout 1s" in outcome.failure["message"]
    # 消息如实（评审 I-1）：Windows=硬终止不得谎称 SIGTERM；POSIX=SIGTERM 致死
    if os.name == "nt":
        assert "SIGTERM" not in outcome.failure["message"]
        assert "terminated (hard-kill" in outcome.failure["message"]
    else:
        assert "SIGTERM sent (exit=-15)" in outcome.failure["message"]
    assert time.monotonic() - started < 10  # 确实被终止了，不是陪着睡满 30s


def test_real_subprocess_abort_mid_run(tmp_path):
    scen = make_scenario(tmp_path, "import time\n\ndef test_sleep():\n    time.sleep(30)\n")
    abort = threading.Event()
    threading.Timer(0.4, abort.set).start()
    started = time.monotonic()

    outcome = runner.run_scenario(
        make_task(), abort, scenarios_root=scen,
        work_root=tmp_path / "work", total_timeout_s=30, graceful_s=1.0,
    )

    assert outcome.status == "ABORTED"
    assert outcome.exit_code is not None  # 已收尸，不是悬着的进程
    assert time.monotonic() - started < 10


def test_relative_roots_are_canonicalized(tmp_path, fake_popen, monkeypatch):
    """相对 work_root/scenarios_root 不允许错位：子进程 cwd 换到工作目录后，
    相对路径会在子进程侧二次解析（评审 M-2）——入口处必须转绝对。"""
    proc = fake_popen(FakeProcess(returncode=0))
    preset_results(tmp_path)
    monkeypatch.chdir(tmp_path)

    runner.run_scenario(
        make_task(), threading.Event(), scenarios_root=Path("scen"), work_root=Path("work"), **FAST
    )

    assert Path(proc.cmd[3]).is_absolute()  # 场景目录
    assert Path(proc.cmd[5]).is_absolute()  # --params-path
    assert Path(proc.kwargs["cwd"]).is_absolute()
