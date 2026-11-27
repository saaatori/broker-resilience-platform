"""BRP 执行面 Agent 入口（M2 第 40 天）：注册 → 心跳循环 → 领取执行 → 执行 → 回传。

执行链（executor/execution.py）：STATUS 事件序列 + 实验环境生命周期（envmgr）
+ pytest 子进程（runner）+ 结果回传。取消/失联的中止响应由 AgentLoop 经 AbortSignal 传入。
"""

import functools
import logging
import os
import socket

from executor.client import DEFAULT_BASE_URL, ControlClient
from executor.execution import SCENARIOS_ROOT
from executor.execution import execute as run_execution
from executor.loop import AgentLoop

VERSION = "0.2.0"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] [%(threadName)s] %(message)s",
)
log = logging.getLogger("brp-agent")


def main() -> None:
    base_url = os.environ.get("BRP_API_BASE_URL", DEFAULT_BASE_URL)
    client = ControlClient(base_url)
    execute = functools.partial(run_execution, client=client, scenarios_root=SCENARIOS_ROOT)
    loop = AgentLoop(client, execute, hostname=socket.gethostname(), version=VERSION)
    try:
        loop.run()
    except KeyboardInterrupt:
        loop.stop()


if __name__ == "__main__":
    main()
