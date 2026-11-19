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
- **`lease_ok` 判定规则（v1.0.4 定；v1.0.5 补分支序）**：对上报的每个 eid——**租约失效者一律不入两数组（含 CANCELLING，v1.0.5）**；其余按序：执行存在、归属本 Agent、非终态、租约未过期 → 计入 `lease_ok`；任务处于 CANCELLING → 计入 `cancel_requested`（互斥，取消优先）；其余（不存在/他 Agent/终态）→ 两数组皆不入（落入差集语义）。

### 2.3 `POST /agent/tasks/claim`

原子领取一个待执行任务。

- 请求：`{"agent_id": str}`
- 响应 200：`{"task_id": str, "execution_id": str(uuid), "attempt": int, "scenario_key": str, "params_snapshot": object}`——`task_id` 为**数据库 bigint id 的十进制字符串形式**（v1.0.5 统一口径：与 schema/conftest/容器名的 string 形态一致；M1 手跑的语义化 id 如 "w6accept" 同为合法值）。
- 响应 204：无任务（Agent 继续轮询，默认间隔 5s）。
- 语义：`SELECT … FOR UPDATE SKIP LOCKED` 一次领一个；领取即创建 `test_execution(attempt)` 并写入租约。并发下同任务只有一个领取成功——**SKIP LOCKED 正常路径的失败方收到 204**；**409 为防御性保留**（唯一约束/状态机拒绝等异常路径），处置同为退避重试（v1.0.1 注）。
- **Agent 落盘合成式（v1.0.4 定）**：Agent 将 `params.json` = claim 响应的 `params_snapshot` + 响应顶层 `task_id`/`execution_id` 写至 `/work/<execution_id>/params.json`（conftest 消费面；与 §6.1 合成规则衔接）。

### 2.4 `POST /executions/{eid}/events`

执行事件流上报（SSE 转发给前端）。

- 请求：`{"seq": int, "ts": str(ISO 8601), "type": "STATUS" | "LOG" | "PROGRESS", "payload": object}`
- 响应：**202 Accepted**；幂等键 `(eid, seq)`（重复 seq 静默去重）。
- **seq 发端行为（v1.0.4 定）**：执行内**严格递增、不得重复**（起点不限，样例从 0 起）；重复由幂等去重吸收。
- **乱序语义（v1.0.4 定）**：按到达顺序接受并 SSE 转发，**不重排**；需要顺序的消费者按 seq 自行重排。
- **存储语义（v1.0.4 定，架构护栏；v1.0.5 补有界性与降级）**：events **不入 PG**（决策 6 的"事件不逐行入库"同样适用于本端点）——幂等去重走 **Redis SETNX**（TTL 10 分钟，总体设计表 26 键 `exec:evt:{eid}:{seq}`），SSE 即时转发不持久化。**去重窗口=TTL 10 分钟（有界）**：窗口外的迟到重传可能再次转发——消费者按 seq **重排并去重**（v1.0.5）；Redis 不可用时 SETNX 失败=返回 5xx（Agent 退避重试，不做降级放行，保去重语义强）。**SSE 断线不回放**：事件无持久化源，重连后进度以任务状态查询兜底（M-3 成文，M4 前端注意）。
- **ts 口径（v1.0.1 定，决策 11）**：ts = **事件发生时刻**（非上报时刻），UTC ISO 8601 **带时区后缀**。
- **STATUS 触发语义（v1.0.1 定）**：`DISPATCHED → RUNNING` 由该执行的**首条 `type=STATUS` 事件触发**（不论子状态取值）；STATUS 的 payload 含子状态（载荷键名 **`substate`**，取值集=§5 序列——**v1.0.2 已固化**）。
- **payload 三型（v1.0.2 固化，schema 为权威；v1.0.3 起 type 与 payload 形状在 schema 中绑定——错配即拒收）**：STATUS `{substate}`；LOG `{message}`；PROGRESS `{label?, current, total}`——详见 `docs/protocol/schema/events.schema.json`（顶层 oneOf 三完整分支，各含 type const）。
- 终态后收到 events：记录并返回 2xx、不改状态（§4 兜底，v1.0.1）。

### 2.5 `POST /executions/{eid}/result`

执行结果回传（终态）。

- 请求：`{"attempt": int, "status": "COMPLETED" | "FAILED" | "CANCELLED", "summary": object, "reconciliation": object, "artifacts": [str, ...]}`
- 响应 **200**（v1.0.1 定）；幂等键 `(eid, attempt)`（重复回传幂等落库，不重复生效）。
- `summary` / `reconciliation` 的对象形状**已固化于 `docs/protocol/schema/result.schema.json`**（v1.0.2：键集=counter 六计数+metrics 五指标+verdict/details+差集证据，与 M1 实现 dict 键一致——校验测试钉防漂移）；`artifacts` 为**归档卷挂载点下的相对路径**列表（JSONL zip 等）——Agent 将文件置于共享归档卷（部署前提=实施规划决策 6 归档卷，v1.0.1 定），Go 侧按此路径登记 `test_report.file_path`。
- 终态后重复/迟到 result：幂等落库、返回 200、不改状态（§4 兜底）。

### 2.6 `POST /executions/{eid}/cleanup-ack`

清理确认（取消/失联/完成后的环境清理闭环）。

- 请求：`{"cleaned": bool, "leftovers": [str, ...]}`
- 响应 **200**（v1.0.1 定）。
- 语义：Agent 报告实验环境清理结果；`leftovers` 非空时 Go 侧记入清理任务队列兜底。
- **非取消态语义（v1.0.4 定）**：cleanup-ack 仅在 CANCELLING 有状态转移语义；其余任何状态（含 RUNNING/DISPATCHED 与终态）收到 = 记录、返回 200、**不改状态**。
- **时序（v1.0.1 定，见 §4 取消闭环时序）**：取消/失联路径 = 先 cleanup-ack（触发状态转移）后 result；cleanup-ack 失败可重试（幂等）。

## 3. 错误约定

| 状态码 | 语义 | Agent 侧处置义务 |
| --- | --- | --- |
| 401 | token 无效 | 重新走 register 换取新 token（同 hostname 复用 agent_id） |
| 409 | 领取冲突（防御性保留，正常路径失败方收 204） | 退避后重试轮询 claim（上限 30s） |
| 410 | 租约失效（由 claim/heartbeat 返回，Agent 级；执行级经 lease_ok 差集） | **必须中止当前执行**并走恢复清理（recover+cleanup），随后 cleanup-ack；破坏性任务不自动重跑 |

**执行归属（v1.0.5）**：`/executions/{eid}` 三端点（events/result/cleanup-ack）限**本 Agent 名下**的执行——非本 Agent 的 eid 视同不存在（404）。

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
| DISPATCHED | 收到 result(COMPLETED/FAILED)※※ | COMPLETED/FAILED | v1.0.4 扩展：Agent 在首条 STATUS 前崩溃恢复后直接回传的场景；动作同 RUNNING 对应行 |
| CANCELLING | 收到 result(COMPLETED/FAILED)※※ | CANCELLED | v1.0.4 扩展：取消意图优先——result 照录但不改变取消终态；清理照常兜底 |
| DISPATCHED / RUNNING / CANCELLING ※ | 租约过期（45s 无心跳） | FAILED | 标记 lease_lost；破坏性任务不自动重跑，仅排队清理任务 |

**取消闭环时序（v1.0.1 定，C-1）**——两个实现者的唯一读法：

```
cancel_requested（心跳响应下发）
  → Agent 中止执行（子状态 ABORTED）
  → recover + cleanup（恢复管理器兜底清理）
  → cleanup-ack                    ← 触发 CANCELLING → CANCELLED（正常路径）
  → result(CANCELLED)              ← 幂等落库，不改状态（迟到/重复同样 200）
```

**用户取消入口 × 各状态（v1.0.5 定，I-2——取消 REST 是前端→Go，不在 6 个 Agent 端点内）**：PENDING 取消 = 直接置 CANCELLED（无执行清理负担）；DISPATCHED 取消 = 置取消标志（Redis `exec:cancel:{eid}`），**首条 STATUS 事件到达即转 CANCELLING**，随心跳下发 cancel_requested 走闭环（Agent 失联则清扫兜底）；RUNNING 起按表行 5。前端取消按钮无需按状态禁用。

**result(CANCELLED) 于非取消态（v1.0.5 定，I-1）**：Agent 自行中止（无用户取消）在 DISPATCHED/RUNNING 态回传 result(CANCELLED) = **照录不改态**，由租约清扫收口为 FAILED(lease_lost)——失败归因保持"失联/中止"，不与用户取消混淆。

**CANCELLING 收 result(COMPLETED/FAILED) 的照录细则（v1.0.5 定，I-4）**：test_result 保留**原 status**（COMPLETED/FAILED 照录）；报告与 artifacts 登记照 RUNNING 完成路径执行；幂等 **first-wins**（`uq(execution_id)` 冲突即忽略后续——含闭环尾随的 result(CANCELLED)）。

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

**合成规则（第 26 天定稿）**：`params_snapshot` = 场景默认参数（`lab/scenarios/<key>/params.default.json`；day31 起为 `test_scenario.default_params` 的 seed）+ 运行期注入（`task_id` / `execution_id` / `instance_ref`，runner 落盘至 `/work/<execution_id>/params.json`）。结构已固化为 `docs/protocol/schema/params_snapshot.schema.json`（v1.0.2；注入键为可选属性——claim 响应与 params.json 文件共用一份 schema）。

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
- **v1.0.2（2026-11-19，schema 固化）**：JSON Schema 三件入库 `docs/protocol/schema/`（params_snapshot/events/result + samples 三件 + 校验测试入 agent 套件）：①events payload 三型定义（STATUS `{substate}` / LOG `{message}` / PROGRESS `{label?, current, total}`，oneOf 互斥）——子状态载荷键名固化 `substate`；②result 的 summary/reconciliation 键集固化（=counter/metrics 实现 dict 键）；③params 结构固化（注入键=可选属性，两形态共用 schema）。schema 为上述细化项的**权威**，本文档指向之。
- **v1.0.3（2026-11-19，schema 评审补丁）**：①events schema 改**顶层 oneOf 三完整分支**（type const 绑定 payload 形状）——修复 v1.0.2 的 type↔payload 错配放行（评审实测 LOG+substate/STATUS+message 均曾通过）；②result.details.actual 收窄为 number|null（无 string 生产者）；③details.metric 收紧为五值枚举（与断言侧对称）；④samples 补 LOG/PROGRESS 两件；⑤**语法口径**：三件 schema 为 draft-07 与 2020-12 公共子集（无 $defs/$ref/prefixItems 等独有关键字）——Go 侧库选型不受 draft 限制。
- **v1.0.4（2026-11-19，双视角走查修订）**：Go/Agent 双实现者 dry-run 走查出的八项歧义收口：①状态机扩展两行【※※】——DISPATCHED 收 result（Agent 首条 STATUS 前崩溃恢复场景）与 CANCELLING 收 result(COMPLETED/FAILED)（取消意图优先，result 照录不改终态）；②events seq 发端=严格递增不得重复；③events 乱序=接受并转发不重排（消费者按 seq 自行重排）；④events **存储语义护栏**：不入 PG（决策 6 同样适用本端点），幂等去重=Redis SETNX（表 26 键 `exec:evt:{eid}:{seq}`，TTL 10min），SSE 即时转发不持久化；⑤claim 的 **Agent 落盘合成式**成文（params.json = snapshot + 响应顶层 task_id/execution_id → /work/<eid>/）；⑥heartbeat **lease_ok 判定规则**成文（存在/归属/非终态/租约有效；CANCELLING→cancel_requested 互斥取消优先）；⑦cleanup-ack **非取消态语义**（仅 CANCELLING 有转移，其余记录不改态）。走查表见开发日志第 29 天。
- **v1.0.5（2026-11-19，走查评审补丁——第三实现者沿 status 维度与取消触发面重走）**：①**用户取消 × 各状态**成文（PENDING=直接 CANCELLED；DISPATCHED=置标志+首条 STATUS 转 CANCELLING；取消入口在前端→Go，v1.0.4 双视角的结构性盲区）；②result(CANCELLED) 于非取消态=照录不改态、清扫收口（失败归因不混淆）；③CANCELLING 收 result(COMPLETED/FAILED) 照录细则（保留原 status/报告照常/幂等 first-wins）；④task_id 类型统一为 **str**（DB bigint 的字符串化——消除协议 int 与 schema string 的字段级矛盾）；⑤lease_ok 分支序补"租约失效一律不入两数组（含 CANCELLING）"；⑥events 去重窗口有界性（TTL 10min，窗口外重复可能重发；消费者按 seq 重排**并去重**）+ Redis 不可用=5xx 不降级 + **SSE 断线不回放**（任务状态查询兜底）；⑦三 `/executions/*` 端点限本 Agent 名下执行（否则 404）。**走查表↔本日志编号映射**：v1.0.4 日志①=走查卡壳⑥⑦（两行）、②=卡壳③、③=卡壳②、④=卡壳⑧、⑤=卡壳④、⑥=卡壳⑤、⑦=卡壳⑦。
