"""recovery/orphan.py 启动孤儿清扫（M2 第 42 天，实施规划决策 10 / §7 清理三保险之③）。

Agent 进程被杀（容器随 restart 策略自动复活）时，其执行线程俱亡、在跑表清空，
但实验容器/网络/卷（独立容器）残留。重启后按 label 键存在性扫全部实验资源，
按 brp.execution 分组，keep 白名单外逐组走 lab_factory.down 强清（label 白名单销毁，决策 8）。

单 Agent 单机语义：进程死亡时所有实验资源都是孤儿——keep 参数为多并发/测试留口子。
时序注记：清扫时所属任务可能仍是 RUNNING（租约未到 45s）——执行进程已死、结果永不来，
先清容器无协议副作用，Go 照常收口 FAILED(lease_lost)；cleanup:queue 条目（清扫器入队）
无消费者（day43），与本地强清幂等共存。

误清面双闸：①docker label 过滤只扫 brp.task 键存在的资源；②分组键要求 brp.task 与
brp.execution 两键齐备——缺 execution 的脏资源**静默跳过并告警**（不猜归属，宁漏勿错记档）。
"""

from __future__ import annotations

import logging

from envmgr import docker_client as dc
from envmgr import lab_factory

log = logging.getLogger("brp-agent")


def sweep_orphans(docker_client, keep_eids: frozenset[str] | set[str] = frozenset()) -> dict:
    """扫全部实验资源并强清孤儿组；返回 {task_id: 清掉的组数}。

    keep_eids：白名单内的 execution 组不动（当前活跃执行）。
    单组清理失败记日志继续（下一组不受影响；残留等 day43 队列消费兜底）。
    """
    groups: set[tuple[str, str]] = set()
    skipped = 0  # 缺分组键的脏资源计数（告警不留盲区）

    def group(labels: dict) -> None:
        nonlocal skipped
        task_id = labels.get(dc.LABEL_TASK, "")
        eid = labels.get(dc.LABEL_EXECUTION, "")
        if task_id and eid:
            groups.add((task_id, eid))
        elif labels:
            skipped += 1

    for container in docker_client.containers.list(all=True, filters={"label": dc.LABEL_TASK}):
        group(container.labels)
    for volume in docker_client.volumes.list(filters={"label": dc.LABEL_TASK}):
        # docker-py 的 Volume 无 .labels 属性（labels 在 attrs["Labels"]）——e2e 实测坑
        group(volume.attrs.get("Labels") or {})
    for network in docker_client.networks.list(filters={"label": dc.LABEL_TASK}):
        # docker-py 的 Network 同样无 .labels（attrs["Labels"]；只有 Container 有 .labels）
        group(network.attrs.get("Labels") or {})

    if skipped:
        log.warning("孤儿清扫跳过 %d 个缺 brp.execution 标签的脏资源（不猜归属，人工核查）", skipped)

    swept: dict[str, int] = {}
    for (task_id, eid) in sorted(groups):
        if eid in keep_eids:
            continue
        try:
            lab_factory.down(docker_client, task_id, eid)
            swept[task_id] = swept.get(task_id, 0) + 1
            log.info("孤儿清扫：task=%s exec=%s 已清除", task_id, eid)
        except Exception as e:  # noqa: BLE001 — 单组失败不阻断其余（残留有 day43 队列兜底）
            log.warning("孤儿清扫失败（跳过该组，残留由清理队列兜底）：task=%s exec=%s：%s",
                        task_id, eid, e)
    return swept
