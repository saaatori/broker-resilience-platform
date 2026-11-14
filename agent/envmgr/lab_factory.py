"""实验环境工厂（M1 第 17 天）：渲染 lab-template → docker SDK 拉起 brp_<taskId> 容器组。

- 双文件成对渲染（lab-template.yml + toxiproxy.json，troubleshooting #14）+ 占位符零残留断言；
- 模板是唯一事实源：限额/label/healthcheck 全部读自渲染解析结果，代码不复制这些值；
- 落盘渲染对自洽：proxy 挂载源重写为渲染版 toxiproxy.json 绝对路径（可手动 compose 复盘）；
- get-or-create 幂等：上次起一半失败，重跑补齐不冲突；
- down 只按 label 白名单销毁（envmgr.docker_client），绝不按裸名字删。
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from docker.errors import NotFound

from envmgr import docker_client as dc

REPO_ROOT = Path(__file__).resolve().parents[2]
TEMPLATE_COMPOSE = REPO_ROOT / "deploy" / "compose" / "lab-template.yml"
TEMPLATE_TOXIPROXY = REPO_ROOT / "deploy" / "lab" / "toxiproxy.json"

PLACEHOLDERS = ("<TASK_ID>", "<EXECUTION_ID>", "<NET_NAME>")
NS_PER_SECOND = 1_000_000_000
TOXIPROXY_MOUNT_TARGET = "/etc/toxiproxy/toxiproxy.json"
HEALTHY_MARGIN_S = 15.0  # 推导健康等待时的固定余量

log = logging.getLogger("brp-lab-factory")


@dataclass
class ServiceSpec:
    """从渲染后的 compose 解析出的单容器创建参数（模板是唯一事实源）。"""

    name: str
    role: str
    image: str
    command: list[str] | None
    labels: dict[str, str]
    mem_limit: str
    nano_cpus: int
    healthcheck: dict | None
    mounts: list[dict] = field(default_factory=list)
    network: str = ""


@dataclass
class RenderedLab:
    task_id: str
    execution_id: str
    compose_path: Path
    toxiproxy_path: Path
    net_name: str
    volume_name: str
    network_labels: dict[str, str]
    services: list[ServiceSpec]


def _to_ns(duration: str) -> int:
    """compose 时长（"5s"/"1m"）转纳秒整数（docker-py healthcheck 口径）。

    不支持的格式（复合时长/无单位/未知单位）报 RuntimeError 点名输入值，而非裸 ValueError。
    """
    unit = duration[-1:]
    try:
        value = float(duration[:-1])
    except ValueError:
        value = None
    if value is None or unit not in ("s", "m", "h"):
        raise RuntimeError(f"unsupported duration {duration!r}: expected '<n>s' / '<n>m' / '<n>h'")
    return int(value * {"s": 1, "m": 60, "h": 3600}[unit] * NS_PER_SECOND)


def _rewrite_toxiproxy_mount(volumes: list[str], toxiproxy_path: Path) -> list[str]:
    """仅重写指向 toxiproxy 配置目标的挂载源（渲染件绝对路径），保留其余卷与其模式。

    模板是唯一事实源：proxy 将来加第二条卷不得被静默吞掉（评审 M-6）。
    """
    rewritten = []
    for vol in volumes:
        parts = vol.rsplit(":", 2) if vol.endswith((":ro", ":rw")) else vol.rsplit(":", 1)
        target = parts[-2] if len(parts) == 3 else parts[-1]
        if target == TOXIPROXY_MOUNT_TARGET:
            mode = f":{parts[-1]}" if len(parts) == 3 else ""
            vol = f"{toxiproxy_path.resolve().as_posix()}:{TOXIPROXY_MOUNT_TARGET}{mode}"
        rewritten.append(vol)
    return rewritten


def _parse_volume(vol: str, volumes_map: dict) -> dict:
    """compose 卷串转 SDK mount 字典；命名卷键解析为实际卷名。

    从右侧 split（bind 源在 Windows 下可能是 "F:/..." 带盘符冒号）。
    """
    parts = vol.rsplit(":", 2) if vol.endswith((":ro", ":rw")) else vol.rsplit(":", 1)
    source, target = parts[0], parts[-2] if len(parts) == 3 else parts[-1]
    read_only = vol.endswith(":ro")
    if source in volumes_map:  # 顶层命名卷键 → volume 挂载
        return {
            "source": volumes_map[source]["name"],
            "target": target,
            "type": "volume",
            "read_only": read_only,
        }
    return {"source": source, "target": target, "type": "bind", "read_only": read_only}


def _spec(svc: dict, net_name: str, volumes_map: dict) -> ServiceSpec:
    # 模板契约：restart 一律 "no"（M3 依赖：故障策略 kill 的容器不得被自动复活）。
    # 非 "no" 在此 fail-fast——SDK 默认即 no，若将来放行其他值须显式翻译而非静默忽略。
    restart = svc.get("restart", "no")
    if restart != "no":
        raise RuntimeError(
            f"service {svc.get('container_name', '?')} declares restart={restart!r}: "
            f"lab template only allows \"no\""
        )
    limits = svc.get("deploy", {}).get("resources", {}).get("limits", {})
    name = svc.get("container_name", "?")
    if "memory" not in limits or "cpus" not in limits:
        raise RuntimeError(
            f"service {name}: deploy.resources.limits 必填 memory 与 cpus（契约：容器资源限额必填）"
        )
    hc = svc.get("healthcheck")
    return ServiceSpec(
        name=svc["container_name"],
        role=svc["labels"][dc.LABEL_ROLE],
        image=svc["image"],
        command=svc.get("command"),
        labels=dict(svc.get("labels") or {}),
        mem_limit=limits["memory"],
        nano_cpus=int(float(limits["cpus"]) * NS_PER_SECOND),
        healthcheck=(
            {
                "test": hc["test"],
                "interval": _to_ns(hc["interval"]),
                "timeout": _to_ns(hc["timeout"]),
                "retries": hc["retries"],
                "start_period": _to_ns(hc.get("start_period", "0s")),
            }
            if hc
            else None
        ),
        mounts=[_parse_volume(v, volumes_map) for v in svc.get("volumes") or []],
        network=net_name,
    )


def render_lab(task_id: str, execution_id: str, out_dir: Path | str) -> RenderedLab:
    """渲染双文件到 out_dir 并解析为 SDK 规格；占位符零残留 fail-fast。"""
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    net_name = f"brp_task_{task_id}"
    subs = {"<TASK_ID>": task_id, "<EXECUTION_ID>": execution_id, "<NET_NAME>": net_name}

    yml_text = TEMPLATE_COMPOSE.read_text(encoding="utf-8")
    json_text = TEMPLATE_TOXIPROXY.read_text(encoding="utf-8")
    for ph, value in subs.items():
        yml_text = yml_text.replace(ph, value)
        json_text = json_text.replace(ph, value)
    for artifact, where in ((yml_text, "lab-template.yml"), (json_text, "toxiproxy.json")):
        for ph in PLACEHOLDERS:
            if ph in artifact:
                raise RuntimeError(f"render residue: {ph} still present in rendered {where}")

    toxiproxy_path = out_dir / "toxiproxy.json"
    toxiproxy_path.write_text(
        json.dumps(json.loads(json_text), indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )

    cfg = yaml.safe_load(yml_text)
    # proxy 挂载源重写为渲染版 json 绝对路径（渲染对自洽：手动 docker compose -f 亦可用；
    # 仅动 toxiproxy 目标那条，其余卷保留——模板是唯一事实源）
    cfg["services"]["proxy"]["volumes"] = _rewrite_toxiproxy_mount(
        cfg["services"]["proxy"]["volumes"], toxiproxy_path
    )
    compose_path = out_dir / "lab-compose.yml"
    compose_path.write_text(
        yaml.safe_dump(cfg, sort_keys=False, allow_unicode=True), encoding="utf-8"
    )

    final = yaml.safe_load(compose_path.read_text(encoding="utf-8"))  # 以落盘件为准
    volumes_map = final.get("volumes") or {}
    net_cfg = final["networks"]["lab-net"]
    return RenderedLab(
        task_id=task_id,
        execution_id=execution_id,
        compose_path=compose_path,
        toxiproxy_path=toxiproxy_path,
        net_name=net_cfg["name"],
        volume_name=volumes_map["jsonl"]["name"],
        network_labels=dict(net_cfg.get("labels") or {}),
        services=[_spec(svc, net_cfg["name"], volumes_map) for svc in final["services"].values()],
    )


def _ensure_labels(kind: str, name: str, actual: dict, expected: dict) -> None:
    """get-or-create 命中存量资源时校验 label：不一致即拒绝收养（评审 I-1）。

    同任务新 attempt 撞上上次崩溃的旧 execution 资源时，静默复用会让
    down(execution=新) 按 label 匹配不到任何资源而泄漏——fail-fast 优于自动重建。
    """
    for key, want in expected.items():
        got = actual.get(key)
        if got != want:
            raise RuntimeError(
                f"{kind} {name} exists with label {key}={got!r}, expected {want!r}: "
                f"stale resource from another execution, refusing to adopt (clean it first)"
            )


def _get_or_create_network(client, rendered: RenderedLab):
    try:
        net = client.networks.get(rendered.net_name)
        _ensure_labels("network", net.name, net.labels, rendered.network_labels)
        return net
    except NotFound:
        return client.networks.create(rendered.net_name, labels=rendered.network_labels)


def _get_or_create_volume(client, rendered: RenderedLab):
    labels = {dc.LABEL_TASK: rendered.task_id, dc.LABEL_EXECUTION: rendered.execution_id}
    try:
        volume = client.volumes.get(rendered.volume_name)
        _ensure_labels("volume", volume.name, volume.labels, labels)
        return volume
    except NotFound:
        # 命名卷不进模板字段，但白名单纪律要求可按 label 找到它（down 用）
        return client.volumes.create(rendered.volume_name, labels=labels)


def _get_or_create_container(client, spec: ServiceSpec):
    try:
        container = client.containers.get(spec.name)
        _ensure_labels("container", container.name, container.labels, spec.labels)
        return container
    except NotFound:
        return client.containers.create(
            spec.image,
            spec.command,
            name=spec.name,
            labels=spec.labels,
            network=spec.network,
            mem_limit=spec.mem_limit,
            nano_cpus=spec.nano_cpus,
            healthcheck=spec.healthcheck,
            mounts=spec.mounts,
        )


def _wait_healthy(client, spec: ServiceSpec, timeout_s: float, poll_interval_s: float) -> None:
    deadline = time.monotonic() + timeout_s
    while True:
        container = client.containers.get(spec.name)
        container.reload()
        status = container.attrs["State"].get("Health", {}).get("Status")
        if status == "healthy":
            return
        if status == "unhealthy":
            raise RuntimeError(f"container {spec.name} became unhealthy while waiting")
        if time.monotonic() >= deadline:
            raise RuntimeError(
                f"container {spec.name} not healthy within {timeout_s}s (status={status})"
            )
        time.sleep(poll_interval_s)


def _derived_healthy_timeout_s(spec: ServiceSpec) -> float:
    """按模板声明的 healthcheck 推导等待上限：start_period + retries×interval + 余量。

    broker 模板值 = 15s + 12×5s + 15s = 90s；固定 60s 默认会在引擎尚未判定前假阴性超时。
    """
    hc = spec.healthcheck
    return (hc["start_period"] + hc["retries"] * hc["interval"]) / NS_PER_SECOND + HEALTHY_MARGIN_S


def up(client, rendered: RenderedLab, healthy_timeout_s: float | None = None,
       poll_interval_s: float = 2.0) -> dict:
    """拉起实验环境（get-or-create 幂等），等待声明的 healthcheck 全部转 healthy。

    healthy_timeout_s 缺省时按各服务模板 healthcheck 推导（模板是唯一事实源）。
    """
    net = _get_or_create_network(client, rendered)
    volume = _get_or_create_volume(client, rendered)
    for spec in rendered.services:
        container = _get_or_create_container(client, spec)
        if container.attrs["State"]["Status"] != "running":
            container.start()
    for spec in rendered.services:
        if spec.healthcheck:
            timeout = (
                healthy_timeout_s
                if healthy_timeout_s is not None
                else _derived_healthy_timeout_s(spec)
            )
            _wait_healthy(client, spec, timeout, poll_interval_s)
    log.info(
        "lab up: task=%s net=%s volume=%s containers=%d",
        rendered.task_id, net.name, volume.name, len(rendered.services),
    )
    return {
        "network": net.name,
        "volume": volume.name,
        "containers": [s.name for s in rendered.services],
    }


def down(client, task_id: str, execution_id: str | None = None) -> dict:
    """按 label 白名单销毁：容器 → 卷 → 网络（全清，实验栈不留数据）。"""
    containers = dc.lab_containers(client, task_id, execution_id)
    for c in containers:
        c.remove(force=True)
    volumes = dc.lab_volumes(client, task_id, execution_id)
    for v in volumes:
        v.remove()
    networks = dc.lab_networks(client, task_id, execution_id)
    for n in networks:
        n.remove()
    result = {
        "containers": len(containers),
        "volumes": len(volumes),
        "networks": len(networks),
    }
    log.info("lab down: task=%s removed=%s", task_id, result)
    return result


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        prog="envmgr.lab_factory", description="BRP experiment lab lifecycle (render + SDK)"
    )
    sub = parser.add_subparsers(dest="command", required=True)
    p_up = sub.add_parser("up", help="render template and start brp_<task_id> containers")
    p_up.add_argument("task_id")
    p_up.add_argument("execution_id")
    p_up.add_argument("--out-dir", default=None, help="渲染输出目录（默认 work/brp_<task_id>/）")
    p_up.add_argument(
        "--healthy-timeout-s",
        type=float,
        default=None,
        help="健康等待上限秒（默认按模板 healthcheck 推导：start_period+retries×interval+15s）",
    )
    p_up.add_argument("--poll-interval-s", type=float, default=2.0)
    p_down = sub.add_parser("down", help="remove lab resources by brp label whitelist")
    p_down.add_argument("task_id")
    p_down.add_argument("--execution-id", default=None)

    args = parser.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

    try:
        client = dc.get_client()
        if args.command == "up":
            out_dir = Path(args.out_dir) if args.out_dir else REPO_ROOT / "work" / f"brp_{args.task_id}"
            rendered = render_lab(args.task_id, args.execution_id, out_dir)
            up(client, rendered, args.healthy_timeout_s, args.poll_interval_s)
        else:
            down(client, args.task_id, args.execution_id)
        return 0
    except Exception as exc:  # noqa: BLE001 —— CLI 边界统一转退出码 2（与 setup_broker 同口径）
        log.error("lab %s failed: %s: %s", args.command, type(exc).__name__, exc)
        return 2


if __name__ == "__main__":
    sys.exit(main())
