"""recovery/orphan.py 单测（第 42 天）：启动孤儿清扫——按 label 键存在性扫全部实验资源。

Fake docker 只模拟本模块用到的面：containers/networks/volumes 的 list(filters={"label": "brp.task"})。
平台对照资源（无 label）必须原封不动。
"""


from recovery import orphan


class FakeOrphanContainer:
    def __init__(self, name, labels):
        self.name = name
        self.labels = dict(labels)
        self.removed = False

    def remove(self, force=False):
        self.removed = True


class FakeOrphanVolume:
    """真实 SDK 形态：Volume 无 .labels，labels 在 attrs["Labels"]（e2e 实测坑）。"""

    def __init__(self, name, labels):
        self.name = name
        self.attrs = {"Labels": dict(labels)}
        self.removed = False

    def remove(self):
        self.removed = True


class FakeOrphanNetwork:
    """真实 SDK 形态：Network 同样无 .labels（attrs["Labels"]）。"""

    def __init__(self, name, labels):
        self.name = name
        self.attrs = {"Labels": dict(labels)}
        self.removed = False

    def remove(self):
        self.removed = True


def _label_match(labels: dict, label_filter) -> bool:
    """docker engine 的 label 过滤语义：'key'=存在性；'key=value'=键值匹配；列表=AND。"""
    if label_filter is None:
        return True
    items = label_filter if isinstance(label_filter, list) else [label_filter]
    for item in items:
        if "=" in item:
            key, value = item.split("=", 1)
            if labels.get(key) != value:
                return False
        elif item not in labels:
            return False
    return True


class FakeOrphanDocker:
    """containers/networks/volumes 各带 list(label 过滤)——模拟 docker engine 的 label 过滤语义。"""

    def __init__(self, containers=(), volumes=(), networks=()):
        self._containers = list(containers)
        self._volumes = list(volumes)
        self._networks = list(networks)
        self.containers = self._Containers(self)
        self.networks = self._Networks(self)
        self.volumes = self._Volumes(self)

    class _Containers:
        def __init__(self, parent):
            self._parent = parent

        def list(self, all=True, filters=None):
            return [c for c in self._parent._containers if _label_match(c.labels, filters.get("label"))]

    class _Networks:
        def __init__(self, parent):
            self._parent = parent

        def list(self, filters=None):
            return [n for n in self._parent._networks
                    if _label_match(n.attrs.get("Labels") or {}, filters.get("label"))]

    class _Volumes:
        def __init__(self, parent):
            self._parent = parent

        def list(self, filters=None):
            return [v for v in self._parent._volumes
                    if _label_match(v.attrs.get("Labels") or {}, filters.get("label"))]


def lab_container(task, eid, role):
    return FakeOrphanContainer(
        f"brp_{task}_{role}", {"brp.task": task, "brp.execution": eid, "brp.role": role}
    )


def platform_container(name):
    return FakeOrphanContainer(name, {})


def full_lab(task, eid):
    """一组完整实验资源：五容器 + 一卷 + 一网。"""
    return (
        [lab_container(task, eid, r) for r in ("broker", "proxy", "producer", "consumer", "recorder")],
        [FakeOrphanVolume(f"brp_{task}_jsonl", {"brp.task": task, "brp.execution": eid})],
        [FakeOrphanNetwork(f"brp_task_{task}", {"brp.task": task, "brp.execution": eid})],
    )


def test_sweep_clears_all_lab_groups():
    c1, v1, n1 = full_lab("7", "eid-a")
    c2, v2, n2 = full_lab("8", "eid-b")
    platform = [platform_container("brp-agent"), platform_container("brp-go")]
    client = FakeOrphanDocker(c1 + c2 + platform, v1 + v2, n1 + n2)

    stats = orphan.sweep_orphans(client)

    assert all(c.removed for c in c1 + c2)
    assert all(v.removed for v in v1 + v2)
    assert all(n.removed for n in n1 + n2)
    assert not any(c.removed for c in platform)  # 无 label 的平台容器绝不动
    assert stats == {"7": 1, "8": 1}  # task_id → 清掉的组数（execution 数）


def test_sweep_respects_keep_whitelist():
    c1, v1, n1 = full_lab("7", "eid-a")
    c2, v2, n2 = full_lab("8", "eid-b")
    client = FakeOrphanDocker(c1 + c2, v1 + v2, n1 + n2)

    stats = orphan.sweep_orphans(client, keep_eids={"eid-a"})

    assert not any(c.removed for c in c1) and not any(v.removed for v in v1) and not n1[0].removed
    assert all(c.removed for c in c2) and n2[0].removed
    assert stats == {"8": 1}


def test_sweep_empty_environment_is_noop():
    client = FakeOrphanDocker([platform_container("brp-redis")], [], [])

    stats = orphan.sweep_orphans(client)

    assert stats == {}


def test_sweep_grouping_by_execution_label():
    """同 task 两个 execution 的残留（历史脏数据）：按 eid 分组各自清。"""
    ca, va, _ = full_lab("9", "eid-x")
    cb, vb, nb = full_lab("9", "eid-y")
    client = FakeOrphanDocker(ca + cb, va + vb, nb)

    stats = orphan.sweep_orphans(client)

    assert stats == {"9": 2}
    assert all(c.removed for c in ca + cb)

def test_sweep_catches_network_and_volume_only_residue():
    """容器已不在、只剩网络/卷的残留也要被扫到（分组不依赖容器存在）。"""
    orphan_net = FakeOrphanNetwork("brp_task_11", {"brp.task": "11", "brp.execution": "eid-n"})
    orphan_vol = FakeOrphanVolume("brp_11_jsonl", {"brp.task": "11", "brp.execution": "eid-v"})
    client = FakeOrphanDocker([], [orphan_vol], [orphan_net])

    stats = orphan.sweep_orphans(client)

    assert orphan_net.removed and orphan_vol.removed
    assert stats == {"11": 2}


def test_sweep_continues_after_group_failure(monkeypatch):
    """单组清理失败：记日志继续清下一组（不中断）。"""
    c1, v1, n1 = full_lab("12", "eid-f1")
    c2, v2, n2 = full_lab("13", "eid-f2")
    client = FakeOrphanDocker(c1 + c2, v1 + v2, n1 + n2)

    from recovery import orphan as orphan_mod

    original_down = orphan_mod.lab_factory.down
    calls = []

    def flaky_down(docker, task_id, eid=None):
        calls.append(task_id)
        if task_id == "12":
            raise RuntimeError("network has active endpoints")
        return original_down(docker, task_id, eid)

    monkeypatch.setattr(orphan_mod.lab_factory, "down", flaky_down)

    stats = orphan.sweep_orphans(client)

    assert calls == ["12", "13"]  # 失败后仍尝试下一组
    assert all(c.removed for c in c2) and n2[0].removed  # 第二组清干净
    assert stats == {"13": 1}  # 统计只含成功组

def test_sweep_counts_incomplete_label_resources_as_skipped(caplog):
    """缺 brp.execution 的脏资源：跳过不清、计数告警（宁漏勿错——分组键不齐不猜归属）。"""
    import logging as _logging

    dirty = FakeOrphanContainer("brp_99_mystery", {"brp.task": "99"})  # 无 execution
    c1, v1, n1 = full_lab("14", "eid-ok")
    client = FakeOrphanDocker(c1 + [dirty], v1, n1)

    with caplog.at_level(_logging.WARNING, logger="brp-agent"):
        stats = orphan.sweep_orphans(client)

    assert not dirty.removed  # 脏资源不动
    assert stats == {"14": 1}  # 正常组照清
    assert any("跳过 1 个" in r.getMessage() for r in caplog.records)
