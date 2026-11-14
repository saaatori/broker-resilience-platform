"""lab_factory / docker_client 纯逻辑单测（Fake docker client，不连 daemon）。

Fake 仅模拟 factory 用到的 SDK 面：containers.get/create/list、networks.get/create/list、
volumes.get/create/list、container.start/reload/remove(force)。label 过滤语义与
docker engine 的 label AND 过滤保持一致（docker_client 的白名单纪律赖此成立）。
"""

import json

import pytest
import yaml

from envmgr.lab_factory import down, render_lab, up

# ---------- Fakes ----------


class FakeContainer:
    def __init__(self, name, labels, status="created", health_schedule=None):
        self.name = name
        self.id = "cid-" + name
        self.labels = dict(labels)
        self.create_kwargs = None
        self.started = False
        self.removed = False
        self._status = status
        self._health_schedule = list(health_schedule or [])
        self.attrs = {"State": {"Status": status}}
        if health_schedule is not None:
            self.attrs["State"]["Health"] = {"Status": "starting"}

    def start(self):
        self.started = True
        self._status = "running"
        self.attrs["State"]["Status"] = "running"

    def reload(self):
        if self._health_schedule:
            self.attrs["State"]["Health"] = {"Status": self._health_schedule.pop(0)}

    def remove(self, force=False):
        self.removed = True


class FakeNetwork:
    def __init__(self, name, labels=None):
        self.name = name
        self.labels = dict(labels or {})
        self.removed = False

    def remove(self):
        self.removed = True


class FakeVolume:
    def __init__(self, name, labels=None):
        self.name = name
        self.labels = dict(labels or {})
        self.removed = False

    def remove(self):
        self.removed = True


def _match_labels(labels, filters):
    wanted = filters.get("label", []) if filters else []
    if isinstance(wanted, str):
        wanted = [wanted]
    return all(labels.get(k) == v for k, _, v in (w.partition("=") for w in wanted))


class FakeDockerClient:
    def __init__(self, containers=(), networks=(), volumes=(), health_schedule=None):
        self._containers = list(containers)
        self._networks = list(networks)
        self._volumes = list(volumes)
        self._health_schedule = health_schedule  # 覆盖 create 出的带 healthcheck 容器的序列
        self.created_containers = []
        self.created_networks = []
        self.created_volumes = []

        client = self

        class _Containers:
            def get(self, name):
                for c in client._containers:
                    if c.name == name:
                        return c
                import docker.errors

                raise docker.errors.NotFound(f"no such container {name}")

            def create(self, image, command=None, **kw):
                schedule = None
                if kw.get("healthcheck"):
                    # None=未覆盖（默认一次转 healthy）；[]=恒不 healthy（None 判别，空列表是有效覆盖）
                    schedule = list(
                        client._health_schedule if client._health_schedule is not None else ["healthy"]
                    )
                c = FakeContainer(kw["name"], kw.get("labels") or {}, health_schedule=schedule)
                c.create_kwargs = {"image": image, "command": command, **kw}
                client._containers.append(c)
                client.created_containers.append(c)
                return c

            def list(self, all=False, filters=None):
                matched = [c for c in client._containers if _match_labels(c.labels, filters)]
                # 与真实 SDK 对齐：all=False 只返回运行中容器（防 docker_client 的 all=True 被误删而测不出）
                if all:
                    return matched
                return [c for c in matched if c.attrs["State"]["Status"] == "running"]

        class _Networks:
            def get(self, name):
                for n in client._networks:
                    if n.name == name:
                        return n
                import docker.errors

                raise docker.errors.NotFound(f"no such network {name}")

            def create(self, name, labels=None):
                n = FakeNetwork(name, labels)
                client._networks.append(n)
                client.created_networks.append(n)
                return n

            def list(self, filters=None):
                return [n for n in client._networks if _match_labels(n.labels, filters)]

        class _Volumes:
            def get(self, name):
                for v in client._volumes:
                    if v.name == name:
                        return v
                import docker.errors

                raise docker.errors.NotFound(f"no such volume {name}")

            def create(self, name, labels=None):
                v = FakeVolume(name, labels)
                client._volumes.append(v)
                client.created_volumes.append(v)
                return v

            def list(self, filters=None):
                return [v for v in client._volumes if _match_labels(v.labels, filters)]

        self.containers = _Containers()
        self.networks = _Networks()
        self.volumes = _Volumes()


def make_rendered(tmp_path):
    return render_lab("t17", "eid17", tmp_path / "lab")


# ---------- render ----------


def test_render_replaces_placeholders_in_both_files(tmp_path):
    rendered = make_rendered(tmp_path)

    yml = rendered.compose_path.read_text(encoding="utf-8")
    js = rendered.toxiproxy_path.read_text(encoding="utf-8")
    for ph in ("<TASK_ID>", "<EXECUTION_ID>", "<NET_NAME>"):
        assert ph not in yml, f"yml 残留 {ph}"
        assert ph not in js, f"json 残留 {ph}"

    assert rendered.net_name == "brp_task_t17"
    assert [s.name for s in rendered.services] == [
        f"brp_t17_{r}" for r in ("broker", "proxy", "producer", "consumer", "recorder")
    ]
    cfg = yaml.safe_load(yml)
    assert cfg["networks"]["lab-net"]["name"] == "brp_task_t17"
    assert cfg["volumes"]["jsonl"]["name"] == "brp_t17_jsonl"
    prox = json.loads(js)
    assert prox[0]["upstream"] == "brp_t17_broker:5672"


def test_render_rewrites_proxy_mount_to_rendered_json(tmp_path):
    rendered = make_rendered(tmp_path)

    cfg = yaml.safe_load(rendered.compose_path.read_text(encoding="utf-8"))
    vol = cfg["services"]["proxy"]["volumes"][0]
    expected_src = rendered.toxiproxy_path.resolve().as_posix()
    assert vol == f"{expected_src}:/etc/toxiproxy/toxiproxy.json:ro"


# ---------- up ----------


def test_up_creates_network_volume_and_five_containers(tmp_path):
    client = FakeDockerClient()
    rendered = make_rendered(tmp_path)

    up(client, rendered, healthy_timeout_s=2, poll_interval_s=0)

    assert [(n.name, n.labels) for n in client.created_networks] == [
        ("brp_task_t17", {"brp.task": "t17", "brp.execution": "eid17"})
    ]
    assert [(v.name, v.labels) for v in client.created_volumes] == [
        ("brp_t17_jsonl", {"brp.task": "t17", "brp.execution": "eid17"})
    ]
    assert len(client.created_containers) == 5
    assert all(c.started for c in client.created_containers)

    broker = next(c for c in client.created_containers if c.name == "brp_t17_broker")
    assert broker.create_kwargs["image"] == "rabbitmq:4-management"
    assert broker.create_kwargs["network"] == "brp_task_t17"
    assert broker.create_kwargs["mem_limit"] == "512M"
    assert broker.create_kwargs["nano_cpus"] == 1_000_000_000
    assert broker.create_kwargs["labels"] == {
        "brp.task": "t17",
        "brp.execution": "eid17",
        "brp.role": "broker",
    }
    hc = broker.create_kwargs["healthcheck"]
    assert hc["test"] == ["CMD", "rabbitmq-diagnostics", "-q", "ping"]
    assert hc["interval"] == 5 * 10**9
    assert hc["timeout"] == 5 * 10**9
    assert hc["retries"] == 12
    assert hc["start_period"] == 15 * 10**9

    proxy = next(c for c in client.created_containers if c.name == "brp_t17_proxy")
    rendered_json = rendered.toxiproxy_path.resolve().as_posix()
    assert proxy.create_kwargs["mounts"] == [
        {
            "source": rendered_json,
            "target": "/etc/toxiproxy/toxiproxy.json",
            "type": "bind",
            "read_only": True,
        }
    ]

    recorder = next(c for c in client.created_containers if c.name == "brp_t17_recorder")
    assert recorder.create_kwargs["mounts"] == [
        {"source": "brp_t17_jsonl", "target": "/data", "type": "volume", "read_only": False}
    ]

    helpers = [
        c for c in client.created_containers if c.name.endswith(("_producer", "_consumer"))
    ]
    assert all(c.create_kwargs["mem_limit"] == "256M" for c in helpers)
    assert all(c.create_kwargs["healthcheck"] is None for c in helpers)


def test_up_is_idempotent_under_partial_failure(tmp_path):
    existing_broker = FakeContainer(
        "brp_t17_broker",
        {"brp.task": "t17", "brp.execution": "eid17", "brp.role": "broker"},
        status="running",
        health_schedule=["healthy"],  # 已 healthy：等待立即通过
    )
    client = FakeDockerClient(
        containers=[existing_broker],
        networks=[FakeNetwork("brp_task_t17", {"brp.task": "t17", "brp.execution": "eid17"})],
    )
    rendered = make_rendered(tmp_path)

    up(client, rendered, healthy_timeout_s=2, poll_interval_s=0)

    assert len(client.created_containers) == 4  # 只补缺的四容器
    assert not existing_broker.started  # 存量 running 不重启
    assert not client.created_networks  # 网络已存在不重建


def test_up_waits_for_declared_healthchecks(tmp_path):
    # 恒不 healthy：超时 fail-fast，报错点名容器
    client = FakeDockerClient(health_schedule=[])

    with pytest.raises(RuntimeError, match="brp_t17_broker"):
        up(client, make_rendered(tmp_path), healthy_timeout_s=0.05, poll_interval_s=0)


def test_up_rejects_stale_same_task_container_from_other_execution(tmp_path):
    """同任务异 execution 的同名残留容器：拒收养而非静默复用（评审 I-1）。"""
    stale = FakeContainer(
        "brp_t17_broker",
        {"brp.task": "t17", "brp.execution": "eid-old", "brp.role": "broker"},
        status="running",
        health_schedule=["healthy"],
    )
    client = FakeDockerClient(containers=[stale])

    with pytest.raises(RuntimeError, match="brp.execution"):
        up(client, make_rendered(tmp_path), healthy_timeout_s=2, poll_interval_s=0)


def test_spec_rejects_restart_other_than_no():
    """模板契约 restart 一律 "no"（M3 依赖，kill 不得自动复活）；非 no fail-fast（评审 I-2）。"""
    from envmgr.lab_factory import _spec

    base = {
        "container_name": "x_broker",
        "labels": {"brp.role": "broker"},
        "image": "rabbitmq:4-management",
        "deploy": {"resources": {"limits": {"memory": "512M", "cpus": "1.0"}}},
    }
    _spec(dict(base, restart="no"), "net", {})  # "no" 与缺省都合法
    _spec(dict(base), "net", {})
    with pytest.raises(RuntimeError, match="restart"):
        _spec(dict(base, restart="unless-stopped"), "net", {})


def test_spec_requires_declared_limits():
    """契约"限额必填"：limits 漏配 memory/cpus 须报错，不得静默补默认（评审 M-1）。"""
    from envmgr.lab_factory import _spec

    base = {
        "container_name": "x_broker",
        "labels": {"brp.role": "broker"},
        "image": "rabbitmq:4-management",
    }
    with pytest.raises(RuntimeError, match="x_broker.*limits"):
        _spec(dict(base), "net", {})
    with pytest.raises(RuntimeError, match="limits"):
        _spec(dict(base, deploy={"resources": {"limits": {"memory": "512M"}}}), "net", {})


def test_to_ns_rejects_unsupported_duration_with_clear_error():
    """不支持时长格式报 RuntimeError 点名输入值，而非裸 ValueError（评审 M-1）。"""
    from envmgr.lab_factory import _to_ns

    assert _to_ns("5s") == 5 * 10**9
    assert _to_ns("15s") == 15 * 10**9
    for bad in ("1m30s", "5", "5x"):
        with pytest.raises(RuntimeError, match=bad):
            _to_ns(bad)


def test_derived_healthy_timeout_matches_template_budget(tmp_path):
    """推导公式：start_period(15s) + retries(12)×interval(5s) + 余量(15s) = 90s（评审 M-2）。"""
    import envmgr.lab_factory as lf

    broker = next(s for s in make_rendered(tmp_path).services if s.role == "broker")
    assert lf._derived_healthy_timeout_s(broker) == 90.0


def test_up_default_health_timeout_uses_derivation(tmp_path, monkeypatch):
    """healthy_timeout_s 缺省时走推导值而非固定默认（评审 M-2）。"""
    import envmgr.lab_factory as lf

    monkeypatch.setattr(lf, "_derived_healthy_timeout_s", lambda spec: 0.05)
    client = FakeDockerClient(health_schedule=[])  # broker 恒不 healthy → 走超时路径

    with pytest.raises(RuntimeError, match="within 0.05"):
        up(client, make_rendered(tmp_path), poll_interval_s=0)


def test_rewrite_toxiproxy_mount_preserves_other_volumes(tmp_path):
    """挂载重写只动 toxiproxy 目标那条，其余卷保留、模式保留（评审 M-6）。"""
    from envmgr.lab_factory import _rewrite_toxiproxy_mount

    json_path = tmp_path / "toxiproxy.json"
    json_path.write_text("[]", encoding="utf-8")
    volumes = [
        "../lab/toxiproxy.json:/etc/toxiproxy/toxiproxy.json:ro",
        "extradata:/etc/extra",
        "../lab/toxiproxy.json:/etc/toxiproxy/toxiproxy.json:rw",
    ]

    result = _rewrite_toxiproxy_mount(volumes, json_path)

    expected_src = json_path.resolve().as_posix()
    assert result[0] == f"{expected_src}:/etc/toxiproxy/toxiproxy.json:ro"
    assert result[1] == "extradata:/etc/extra"  # 其他卷原样保留
    assert result[2] == f"{expected_src}:/etc/toxiproxy/toxiproxy.json:rw"  # 模式保留


# ---------- down ----------


def _seed_full_lab(client):
    lab_labels = lambda role: {
        "brp.task": "t17",
        "brp.execution": "eid17",
        "brp.role": role,
    }
    lab = [
        FakeContainer(f"brp_t17_{r}", lab_labels(r), status="running")
        for r in ("broker", "proxy", "producer", "consumer", "recorder")
    ]
    others = [
        FakeContainer("brp_other_broker", {"brp.task": "other", "brp.role": "broker"}),
        FakeContainer("plain-nginx", {}),
    ]
    for c in lab + others:
        client._containers.append(c)
    client._networks.extend(
        [
            FakeNetwork("brp_task_t17", {"brp.task": "t17", "brp.execution": "eid17"}),
            FakeNetwork("platform-net", {}),
        ]
    )
    client._volumes.extend(
        [
            FakeVolume("brp_t17_jsonl", {"brp.task": "t17", "brp.execution": "eid17"}),
            FakeVolume("brp_pgdata", {}),
        ]
    )
    return lab, others


def test_down_removes_only_label_scoped_resources():
    client = FakeDockerClient()
    lab, others = _seed_full_lab(client)

    result = down(client, "t17")

    assert result == {"containers": 5, "volumes": 1, "networks": 1}
    assert all(c.removed for c in lab)
    assert all(not c.removed for c in others)
    assert not client._volumes[1].removed  # brp_pgdata 不动
    assert not client._networks[1].removed  # platform-net 不动


def test_down_unknown_task_is_safe_noop():
    client = FakeDockerClient()
    _seed_full_lab(client)

    result = down(client, "nonexistent")

    assert result == {"containers": 0, "volumes": 0, "networks": 0}
    assert all(not c.removed for c in client._containers)


def test_down_removes_stopped_containers_too():
    """docker_client 查询必须带 all=True（评审 M-3 钉子）：已停止的实验容器也要被清。"""
    client = FakeDockerClient()
    labels = {"brp.task": "t17", "brp.execution": "eid17"}
    client._containers.extend(
        [
            FakeContainer("brp_t17_broker", dict(labels, **{"brp.role": "broker"}), status="running"),
            FakeContainer("brp_t17_proxy", dict(labels, **{"brp.role": "proxy"}), status="exited"),
        ]
    )

    result = down(client, "t17")

    assert result["containers"] == 2
    assert all(c.removed for c in client._containers)


def test_down_filters_by_execution_when_given():
    """down(task, execution_id) 只删该 execution 的资源（评审 M-4 钉子，I-1 修复后的主防线）。"""
    client = FakeDockerClient()

    def lab_c(name, eid, role):
        return FakeContainer(
            name, {"brp.task": "t17", "brp.execution": eid, "brp.role": role}, status="running"
        )

    current = [lab_c(f"brp_t17_{r}", "eid17", r) for r in ("broker", "proxy", "producer")]
    stale = [lab_c("brp_t17_consumer", "eid-old", "consumer"),
             lab_c("brp_t17_recorder", "eid-old", "recorder")]
    client._containers.extend(current + stale)

    result = down(client, "t17", execution_id="eid17")

    assert result == {"containers": 3, "volumes": 0, "networks": 0}
    assert all(c.removed for c in current)
    assert all(not c.removed for c in stale)


def test_wait_healthy_fails_fast_on_unhealthy(tmp_path):
    """healthcheck 转 unhealthy 立即失败，不等超时（评审 M-4 钉子）。"""
    client = FakeDockerClient(health_schedule=["unhealthy"])

    with pytest.raises(RuntimeError, match="unhealthy"):
        up(client, make_rendered(tmp_path), healthy_timeout_s=5, poll_interval_s=0)
