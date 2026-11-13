"""BRP 执行面 Agent（占位骨架，M0 第 13 天）。

职责规划（工程目录结构 §3.2）：注册 → 心跳循环 → 领取执行 → 跑 pytest → 回传结果。
当前仅为容器化占位：保持进程存活并打心跳日志，证明 compose 内 agent 服务稳定运行；
真实主循环（executor/loop.py）自 M2 接入。
"""

import logging
import time

HEARTBEAT_INTERVAL_S = 30

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger("brp-agent")


def main() -> None:
    log.info("BRP agent 骨架启动（占位：M2 接入注册/心跳/领取主循环）")
    while True:
        time.sleep(HEARTBEAT_INTERVAL_S)
        log.info("心跳占位：agent 存活")


if __name__ == "__main__":
    main()
