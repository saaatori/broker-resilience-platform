# BRP Agent Protocol v1.0（冻结）

> Go 控制面 ↔ Python Agent 通信协议。**v1.0 冻结于 2026-11-19（M1 第 27 天）；v1.0.1 同日补丁（wire 要素补全，见文末变更日志）**。
> 来源：实施规划 §4（执行基准）+ 总体设计表 5/表 6（已逐字核对一致）。
> 变更政策：冻结后任何变更须升次版本号（v1.0.x 补丁/v1.1+ 特性）并在文末变更日志记录；M2 实现以本文档为准，实现与文档冲突时先改文档（升版本）再改代码。
> JSON Schema 三件（params_snapshot / events / result）以本文档结构为准生成，第 28 天入库 `docs/protocol/schema/`。

## 1. 通用约定

- **Base URL**：`http://go:8080/api/v1`（生产，服务名解析）；内容类型一律 `application/json`。
- **认证**：Agent 经 `/agent/register` 获取 `agent_token`，此后所有请求携带头 `X-Agent-Token: <token>`；token 无效 → 401。
- **心跳与租约数值**（实施规划决策 3，固定不改）：心跳间隔 **15s**；租约 **45s**（45s 无心跳即判定过期）；**破坏性任务失联永不自动重跑**，只自动排队清理任务。
- **幂等规则**（决策 4）：事件回传幂等键 `(execution_id, seq)`；结果回传幂等键 `(execution_id, attempt)`——Agent 网络重试安全，重复回传由 Go 侧去重。
- **Agent 侧轮询节奏**（v1.0.1 定，Agent 可配）：claim 无任务轮询间隔默认 **5s**；409 退避指数增长、上限 **30s**。
- **未列出状态码兜底**（v1.0.1 定）：其余 4xx（非 401/409/410）**不重试**、记日志告警（如对不存在 eid 发 events 得 404）；5xx 指数退避有限重试。

## 2. 端点（6 个）

### 2.1 `POST /agent/register`

注册 Agent，换取身份与令牌。

- 请求：`{"hostname": str, "version": str, "max_concurrent": int}`
- 响应 200：`{"agent_id": str(uuid), "agent_token": str}`
- 语义：首次注册或重启后重新注册；Go 侧落 `agent_instance`（token 存哈希）。**幂等语义（v1.0.1 定）**：同 `hostname` 重复注册 = **复用同一 agent_id、换发新 token**（旧 token 失效；401 后重新注册走本路径）。

### 2.2 `POST /agent/heartbeat`

心跳兼**取消指令轮询**（省一条独立取消通道）。

- 请求：`{"agent_id": str, "running_executions": [execution_id, ...]}`
- 响应 200：`{"lease_ok": [execution_id, ...], "cancel_requested": [execution_id, ...]}`
- 语义：Go 刷新租约；`lease_ok` 为仍被认可的执行；`cancel_requested` 为要求中止的执行，Agent 须中止并走恢复清理后 cleanup-ack。
- **差集语义（v1.0.1 定）**：Agent 上报的 `running_executions` 中，**既不在 `lease_ok` 也不在 `cancel_requested` 的执行 = 视同 410**（Go 侧已判 FAILED 等），Agent 按 §3 处置（中止+恢复清理+cleanup-ack）。410 由 **claim 与 heartbeat** 返回（Agent 级）；执行级失效经本差集传达。

### 2.3 `POST /agent/tasks/claim`

原子领取一个待执行任务。

- 请求：`{"agent_id": str}`
- 响应 200：`{"task_id": int, "execution_id": str(uuid), "attempt": int, "scenario_key": str, "params_snapshot": object}`
- 响应 204：无任务（Agent 继续轮询，默认间隔 5s）。
- 语义：`SELECT … FOR UPDATE SKIP LOCKED` 一次领一个；领取即创建 `test_execution(attempt)` 并写入租约。并发下同任务只有一个领取成功——**SKIP LOCKED 正常路径的失败方收到 204**；**409 为防御性保留**（唯一约束/状态机拒绝等异常路径），处置同为退避重试（v1.0.1 注）。

### 2.4 `POST /executions/{eid}/events`

执行事件流上报（SSE 转发给前端）。

- 请求：`{"seq": int, "ts": str(ISO 8601), "type": "STATUS" | "LOG" | "PROGRESS", "payload": object}`
- 响应：**202 Accepted**；幂等键 `(eid, seq)`（重复 seq 静默去重）。
- **ts 口径（v1.0.1 定，决策 11）**：ts = **事件发生时刻**（非上报时刻），UTC ISO 8601 **带时区后缀**。
- **STATUS 触发语义（v1.0.1 定）**：`DISPATCHED → RUNNING` 由该执行的**首条 `type=STATUS` 事件触发**（不论子状态取值）；STATUS 的 payload 含子状态（取值集=§5 序列，载荷键名第 28 天 schema 固化）。其余 `payload` 结构按 `type` 区分，**第 28 天 JSON Schema 细化**。
- 终态后收到 events：记录并返回 2xx、不改状态（§4 兜底，v1.0.1）。

### 2.5 `POST /executions/{eid}/result`

执行结果回传（终态）。

- 请求：`{"attempt": int, "status": "COMPLETED" | "FAILED" | "CANCELLED", "summary": object, "reconciliation": object, "artifacts": [str, ...]}`
- 响应 **200**（v1.0.1 定）；幂等键 `(eid, attempt)`（重复回传幂等落库，不重复生效）。
- `summary` / `reconciliation` 的对象形状引用**协议邻接契约**（见 §6，M1 已由 counter/metrics 实现）；`artifacts` 为**归档卷挂载点下的相对路径**列表（JSONL zip 等）——Agent 将文件置于共享归档卷（部署前提=实施规划决策 6 归档卷，v1.0.1 定），Go 侧按此路径登记 `test_report.file_path`。
- 终态后重复/迟到 result：幂等落库、返回 200、不改状态（§4 兜底）。

### 2.6 `POST /executions/{eid}/cleanup-ack`

清理确认（取消/失联/完成后的环境清理闭环）。

- 请求：`{"cleaned": bool, "leftovers": [str, ...]}`
- 响应 **200**（v1.0.1 定）。
- 语义：Agent 报告实验环境清理结果；`leftovers` 非空时 Go 侧记入清理任务队列兜底。
- **时序（v1.0.1 定，见 §4 取消闭环时序）**：取消/失联路径 = 先 cleanup-ack（触发状态转移）后 result；cleanup-ack 失败可重试（幂等）。

## 3. 错误约定

| 状态码 | 语义 | Agent 侧处置义务 |
| --- | --- | --- |
| 401 | token 无效 | 重新走 register 换取新 token（同 hostname 复用 agent_id） |
| 409 | 领取冲突（防御性保留，正常路径失败方收 204） | 退避后重试轮询 claim（上限 30s） |
| 410 | 租约失效（由 claim/heartbeat 返回，Agent 级；执行级经 lease_ok 差集） | **必须中止当前执行**并走恢复清理（recover+cleanup），随后 cleanup-ack；破坏性任务不自动重跑 |

## 4. 任务状态机（Go 侧）

状态集：`PENDING → DISPATCHED → RUNNING →（CANCELLING → CANCELLED ｜ COMPLETED ｜ FAILED）`。

转移表（总体设计表 6 原文 + v1.0.1 两处补全【标 ※】）：

| 当前状态 | 触发事件 | 次状态 | 动作 |
| --- | --- | --- | --- |
| PENDING | Agent 领取成功 | DISPATCHED | 创建 test_execution（attempt）并写入租约 |
| DISPATCHED | 该执行首条 type=STATUS 事件（不论子状态，v1.0.1 澄清） | RUNNING | SSE 广播执行开始 |
| RUNNING | 收到 result(COMPLETED) | COMPLETED | 结果落库、生成报告、推送完成状态 |
| RUNNING | 收到 result(FAILED) | FAILED | 记录失败原因，保留已有证据 |
| RUNNING | 用户取消 | CANCELLING | 取消指令经心跳响应下发 |
| CANCELLING | 收到 cleanup-ack | CANCELLED | SSE 广播取消完成 |
| CANCELLING | 收到 result(CANCELLED)（尚未 cleanup-ack 时）※ | CANCELLED | 视同 cleanup-ack 转移（防卡死双保险，清理仍会执行） |
| DISPATCHED / RUNNING / CANCELLING ※ | 租约过期（45s 无心跳） | FAILED | 标记 lease_lost；破坏性任务不自动重跑，仅排队清理任务 |

**取消闭环时序（v1.0.1 定，C-1）**——两个实现者的唯一读法：

```
cancel_requested（心跳响应下发）
  → Agent 中止执行（子状态 ABORTED）
  → recover + cleanup（恢复管理器兜底清理）
  → cleanup-ack                    ← 触发 CANCELLING → CANCELLED（正常路径）
  → result(CANCELLED)              ← 幂等落库，不改状态（迟到/重复同样 200）
```

**终态兜底规则（v1.0.1 定）**：终态（COMPLETED/FAILED/CANCELLED）后收到 events / result / cleanup-ack：一律记录、返回 2xx、**不改状态**；CANCELLING 态收到 result(CANCELLED) 视同 cleanup-ack 转移（上表 ※ 行）——两条兜底保证任何消息乱序/重试下无卡死路径。

## 5. 执行子状态（Agent 侧，经 events 上报 STATUS）

`CLAIMED → PREPARING → BASELINE → FAULT_INJECTED → RECOVERING → VERIFYING → UPLOADING → DONE/ABORTED`

- 正常场景（无故障）走 `BASELINE → UPLOADING`（跳过故障段）；`ABORTED` 为取消/失联后的中止终态（随后按 §4 取消闭环时序回传）。
- 子状态载荷键名第 28 天 schema 固化；取值集即上述序列（v1.0.1 钉死，供 Go 侧匹配）。

## 6. params_snapshot 与协议邻接契约

### 6.1 params_snapshot 结构（实施规划 §4，含第 26 天补丁）

```
{
  "instance_ref": ...,          // 被测实例引用（M2 建任务时定）
  "message_count": int,
  "publish_rate": int,          // msg/s，0=不限速
  "fault": {                    // M3 故障场景启用；正常场景无 fault 键（见 params.default.json，v1.0.1 措辞）
    "strategy": str, "inject_at_s": int, "duration_s": int
  },
  "observe_window_s": int,      // M3 定稿
  "recovery_timeout_s": int,    // M3 定稿
  "idle_timeout_s": int,        // consumer 排空兜底超时（第 26 天定稿，默认 30）
  "assertions": [ ... ]         // 断言列表（schema 见 6.2）
}
```

**合成规则（第 26 天定稿）**：`params_snapshot` = 场景默认参数（`lab/scenarios/<key>/params.default.json`；day31 起为 `test_scenario.default_params` 的 seed）+ 运行期注入（`task_id` / `execution_id` / `instance_ref`，runner 落盘至 `/work/<execution_id>/params.json`）。

### 6.2 邻接契约（本文档不重复定义，权威指向）

| 契约 | 权威位置 |
| --- | --- |
| 断言 schema `{metric, op, value}`（五 op 白名单；None/越界判定语义） | `agent/reconcile/metrics.py`（docstring 契约）+ `agent/tests/test_metrics.py` |
| 实验事件词汇六词 + 探针前缀（JSONL 行数据契约） | `lab/helpers/events.py`（单一事实源） |
| 场景默认参数（本场景定稿值 10000/500/30/三断言） | `lab/scenarios/normal_delivery/params.default.json`（防漂移钉子：`lab/tests/test_flow_helpers.py`） |
| 对账输出形状（result 的 summary/reconciliation 内容） | `agent/reconcile/counter.py` + `agent/reconcile/metrics.py`（dict 键即契约） |

## 7. 变更日志

- **v1.0（2026-11-19）**：首次冻结。6 端点 + 3 错误码 + 任务状态机（表 6 七转移）+ 执行子状态 + params_snapshot（含 idle_timeout_s 与合成规则）+ 邻接契约四指向。勘误：逐日计划"7 端点"系对总体设计表 5 行数的误计（7 行 = 6 端点 + 错误约定行），按 **6 端点**冻结。
- **v1.0.1（2026-11-19，冻结评审补丁）**：wire 要素补全（评审 1C/7I/6M 处置）。①**取消闭环时序成文**（cleanup-ack 先于 result；CANCELLING 收 result(CANCELLED) 视同转移 ※ 行；终态兜底=记录+2xx+不改状态）；②CANCELLING 并入租约过期转移（→FAILED(lease_lost)）；③result/cleanup-ack 补响应 200；④lease_ok 差集=视同 410 + 410 端点归属（claim/heartbeat，Agent 级；执行级走差集）；⑤DISPATCHED→RUNNING=首条 STATUS 事件触发（不论子状态）+ 子状态取值集钉死；⑥artifacts=归档卷相对路径+共享归档卷部署前提（决策 6）；⑦events.ts=事件发生时刻 UTC 带时区（决策 11）；⑧错误码兜底（其余 4xx 不重试/5xx 退避）+ Agent 轮询默认值（claim 5s/409 上限 30s）；⑨register 幂等（同 hostname 复用 agent_id 换 token）；⑩409 防御性保留注 + SKIP LOCKED 正常路径 204；⑪fault 措辞=正常场景无键。
