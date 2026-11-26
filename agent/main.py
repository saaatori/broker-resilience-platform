"""BRP 执行面 Agent 入口（M2 第 38 天）：注册 → 心跳循环 → 领取执行 → 执行。

执行器尚未接入（runner 第 39 天、端到端第 40 天）：当前领到任务即放弃——不续租约，
任务由控制面在租约过期后收口为 FAILED(lease_lost)，不会卡死。
"""

import logging
import os
import socket

from executor.client import DEFAULT_BASE_URL, ClaimedTask, ControlClient
from executor.loop import AbortSignal, AgentLoop

VERSION = "0.1.0"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] [%(threadName)s] %(message)s",
)
log = logging.getLogger("brp-agent")


def _execute_placeholder(task: ClaimedTask, abort: AbortSignal) -> None:
    """占位执行器：真实执行（runner / 回传）随第 39、40 天接入，接口不变。"""
    log.warning(
        "执行器未接入，执行放弃（租约过期后由控制面收口）：task=%s exec=%s",
        task.task_id,
        task.execution_id,
    )


def main() -> None:
    base_url = os.environ.get("BRP_API_BASE_URL", DEFAULT_BASE_URL)
    client = ControlClient(base_url)
    loop = AgentLoop(client, _execute_placeholder, hostname=socket.gethostname(), version=VERSION)
    try:
        loop.run()
    except KeyboardInterrupt:
        loop.stop()


if __name__ == "__main__":
    main()
