"""场景库根 conftest（M1 第 21 天，实施规划 §6）：params / AMQP 连接 / recorder 地址 fixtures。

**网内前提（架构事实，勿在宿主机直跑场景）**：本 conftest 假设测试进程已在实验网络
`brp_task_<taskId>` 内——生产形态 pytest 跑在 agent 容器内（M2 由 envmgr
network-connect 到任务网）；M1 手跑等价形态=网内容器跑 pytest（实验环境不发布宿主机
端口，宿主机进程解析不了 brp_* 主机名）。环境拉起与 setup_broker 属 runner 职责
（M2=agent；手跑=lab_factory up + setup_broker 幂等先行），本文件零 docker 依赖。

用法（§6 口径）：pytest -p no:cacheprovider scenarios/<key> --params-path params.json
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

# 引导：lab.helpers.* 需要 lab 包的父目录在 sys.path（宿主=仓库根；网内=/repo 挂载根）。
# 保障=祖先 conftest 先于场景测试模块加载（收口时引入，day23 评审确认语义）。
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

DEFAULT_PARAMS_PATH = "params.json"
RECORDER_PORT = 8080
PROXY_AMQP_PORT = 5672  # up 代理（生产路径，§7）
PROXY_AMQP_PORT_DOWN = 5673  # down 代理（消费路径，§7）
DOCKER_CLIENT_TIMEOUT_S = 300  # exec 相邻输出间隔上限（评审 I-2：显式契约而非 docker-py 默认 60s）


def load_params(path: Path | str) -> dict:
    """读 params.json（params_snapshot 落盘件）；缺失/不可读/非法/非 object 一律点名路径 fail-fast。"""
    path = Path(path)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise RuntimeError(f"params file not found: {path}") from None
    except OSError as exc:  # 目录/权限等不可读（FileNotFoundError 已在上面单列）
        raise RuntimeError(f"params file not readable: {path} ({exc})") from None
    except UnicodeDecodeError:
        raise RuntimeError(f"params file {path} is invalid JSON or encoding") from None
    except ValueError:
        raise RuntimeError(f"params file {path} is invalid JSON or encoding") from None
    if not isinstance(data, dict):
        raise RuntimeError(f"params file {path} must contain a JSON object")
    return data


def recorder_url_for(task_id: str) -> str:
    """recorder POST 地址（容器名=主机名，仅实验网内可解析）。"""
    return f"http://brp_{task_id}_recorder:{RECORDER_PORT}/records"


def amqp_parameters(
    host: str,
    port: int = PROXY_AMQP_PORT,
    user: str = "guest",
    password: str = "guest",
    vhost: str = "/",
    socket_timeout: float = 10.0,
):
    """经 Toxiproxy 的 AMQP 连接参数（pika 延迟导入，宿主单测不依赖 pika）。

    socket_timeout（day20 评审 I-3 兑现）：DROP 黑洞下 socket 读不再无限阻塞。
    """
    import pika

    return pika.ConnectionParameters(
        host=host,
        port=port,
        virtual_host=vhost,
        credentials=pika.PlainCredentials(user, password),
        socket_timeout=socket_timeout,
    )


def pytest_addoption(parser):
    parser.addoption(
        "--params-path",
        default=DEFAULT_PARAMS_PATH,
        help="params.json 路径（相对 cwd；M2 runner 落盘于 /work/<execution_id>/，"
        "落盘内容须含 task_id——否则 fixture 缺省 manual 连不上真实实验网）",
    )


@pytest.fixture(scope="session")
def params(request) -> dict:
    return load_params(request.config.getoption("--params-path"))


@pytest.fixture(scope="session")
def task_id(params) -> str:
    return params.get("task_id", "manual")


@pytest.fixture(scope="session")
def recorder_url(task_id: str) -> str:
    return recorder_url_for(task_id)


@pytest.fixture
def amqp_connection(task_id: str):
    """过 Toxiproxy up 代理的 RabbitMQ 连接（每测试独立，teardown 关闭）。

    若报连接失败且主机名形如 brp_<task_id>_proxy：进程不在实验网内——
    场景须在网内跑（见模块 docstring），不是网络故障。
    **注意（day22 评审 I-1）**：fixture 参数在 setup 期即建连，不存在"惰性"——
    长编排场景（exec 等待数十秒以上）勿把本 fixture 挂在测试签名上，闲置连接
    会被 broker 心跳断连；需要时在断言处内联短开短关（复用 amqp_parameters）。
    """
    import pika

    connection = pika.BlockingConnection(amqp_parameters(f"brp_{task_id}_proxy"))
    yield connection
    connection.close()


# ---------- 编排 fixture（M1 第 22 天）：网内 docker exec 实验容器 ----------
# docker 惰性导入：宿主 lab/tests 套件零 docker 依赖；网内跑时容器已 pip install docker。


@pytest.fixture(scope="session")
def lab_containers(task_id: str):
    """按角色取实验容器：lab_containers("producer") → brp_<task_id>_producer 容器对象。

    超时契约（评审 I-2）：client timeout=DOCKER_CLIENT_TIMEOUT_S 是相邻输出的
    间隔上限（流式 helper 持续输出不触发）；helper 静默挂死的总时长防护属
    runner 总超时（§6），不在此层。
    """
    import docker

    client = docker.from_env(timeout=DOCKER_CLIENT_TIMEOUT_S)

    def get(role: str):
        return client.containers.get(f"brp_{task_id}_{role}")

    yield get
    client.close()


@pytest.fixture
def run_helper(lab_containers):
    """在实验容器内执行 helper CLI：run_helper(role, *args) -> (exit_code, output)。

    参数走列表不经 shell；容器不存在时 docker.errors.NotFound 消息含容器名（fail-fast 可自诊）。
    """
    def _run(role: str, *args: str):
        container = lab_containers(role)
        result = container.exec_run(["python", f"/app/lab/helpers/{role}.py", *args])
        return result.exit_code, result.output.decode("utf-8", "replace")

    return _run


@pytest.fixture
def read_jsonl(lab_containers):
    """读 recorder 卷上的 /data/records.jsonl（经容器内 python 输出），返回 list[dict]。"""

    def _read() -> list:
        container = lab_containers("recorder")
        result = container.exec_run(
            [
                "python",
                "-c",
                "import pathlib, sys; p = pathlib.Path('/data/records.jsonl'); "
                "sys.stdout.write(p.read_text(encoding='utf-8') if p.exists() else '')",
            ]
        )
        if result.exit_code != 0:
            raise RuntimeError(
                f"read records.jsonl failed in recorder container: "
                f"{result.output.decode('utf-8', 'replace')}"
            )
        text = result.output.decode("utf-8", "replace")
        events = []
        for line_no, line in enumerate(text.splitlines(), start=1):
            if not line.strip():
                continue
            try:
                events.append(json.loads(line))
            except ValueError:  # 坏行（崩溃尾巴）点名行号——与 recorder 重放同容错口径
                raise RuntimeError(f"records.jsonl corrupt line {line_no}: {line[:100]!r}") from None
        return events

    return _read
