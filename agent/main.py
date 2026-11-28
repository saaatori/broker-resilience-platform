"""BRP 执行面 Agent 入口（M2 第 40 天）：孤儿清扫 → 注册 → 心跳循环 → 领取执行 → 执行 → 回传。

执行链（executor/execution.py）：STATUS 事件序列 + 实验环境生命周期（envmgr）
+ pytest 子进程（runner）+ 结果回传。取消/失联的中止响应由 AgentLoop 经 AbortSignal 传入。
启动孤儿清扫（recovery/orphan.py，day42）：进程被杀后容器自动复活时，清掉上次残留的实验容器组
（在跑表为空 = label 扫到的实验资源全是孤儿）；失败不阻断启动。
"""

import functools
import logging
import os
import socket

from envmgr import docker_client as dc
from executor.client import DEFAULT_BASE_URL, ControlClient
from executor.execution import SCENARIOS_ROOT
from executor.execution import execute as run_execution
from executor.loop import AgentLoop
from recovery import orphan

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
        # 孤儿清扫先于注册（实施规划决策 10）：清扫通常秒级，期间 Agent 尚未上线无碍——
        # 上次执行横竖已死（进程俱亡），Go 侧租约照常过期收口
        try:
            swept = orphan.sweep_orphans(dc.get_client())
            if swept:
                log.info("启动孤儿清扫：清掉 %d 组残留（%s）", sum(swept.values()), swept)
        except Exception:
            log.exception("启动孤儿清扫失败（不阻断启动）")
        loop.run()
    except KeyboardInterrupt:
        loop.stop()


if __name__ == "__main__":
    main()
