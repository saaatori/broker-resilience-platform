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
from pathlib import Path

import pytest

DEFAULT_PARAMS_PATH = "params.json"
RECORDER_PORT = 8080
PROXY_AMQP_PORT = 5672  # up 代理（生产路径，§7）；down 路径直连留给需要它的场景


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
):
    """经 Toxiproxy 的 AMQP 连接参数（pika 延迟导入，宿主单测不依赖 pika）。"""
    import pika

    return pika.ConnectionParameters(
        host=host,
        port=port,
        virtual_host=vhost,
        credentials=pika.PlainCredentials(user, password),
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
    """
    import pika

    connection = pika.BlockingConnection(amqp_parameters(f"brp_{task_id}_proxy"))
    yield connection
    connection.close()
