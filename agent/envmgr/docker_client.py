"""Docker SDK 封装 + brp label 白名单过滤（工程目录 §3.2；实施规划决策 8）。

实验资源的一切查询/销毁都经本模块的 label AND 过滤入口；remove 类操作只应
接收本模块查询返回的对象，绝不按外部传入的裸名字操作容器。
"""

import docker

LABEL_TASK = "brp.task"
LABEL_EXECUTION = "brp.execution"
LABEL_ROLE = "brp.role"


def get_client():
    """按环境（本机 npipe / 容器内 DOCKER_HOST）返回 docker client。"""
    return docker.from_env()


def _label_filters(task_id: str, execution_id: str | None = None) -> list[str]:
    filters = [f"{LABEL_TASK}={task_id}"]
    if execution_id:
        filters.append(f"{LABEL_EXECUTION}={execution_id}")
    return filters


def lab_containers(client, task_id: str, execution_id: str | None = None) -> list:
    """按 label 白名单查实验容器（含已停止的）。"""
    return client.containers.list(
        all=True, filters={"label": _label_filters(task_id, execution_id)}
    )


def lab_networks(client, task_id: str, execution_id: str | None = None) -> list:
    return client.networks.list(filters={"label": _label_filters(task_id, execution_id)})


def lab_volumes(client, task_id: str, execution_id: str | None = None) -> list:
    return client.volumes.list(filters={"label": _label_filters(task_id, execution_id)})
