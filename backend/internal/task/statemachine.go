// statemachine.go 任务状态机决策表（M2 第 36 天）——唯一事实源。
// 协议 v1.0.5 §4 转移表（10 行原文，含 v1.0.1 ※/v1.0.4 ※※ 扩展行）与 v1.0.5 I-1/I-2 的全积编码；
// events.go/result.go（及 day37 清扫、day41 取消、day43 cleanup-ack）一律经 transition() 取转移，
// 不再自写目标态。lease_lost 语义先行入表（触发器=day37 定时清扫）。
package task

// TaskState 任务状态机七态（协议 §4）。
type TaskState string

const (
	StatePending    TaskState = "PENDING"
	StateDispatched TaskState = "DISPATCHED"
	StateRunning    TaskState = "RUNNING"
	StateCancelling TaskState = "CANCELLING"
	StateCompleted  TaskState = "COMPLETED"
	StateFailed     TaskState = "FAILED"
	StateCancelled  TaskState = "CANCELLED"
)

// TaskEvent 状态机输入事件（9 个；来源=各端点与定时器归一后的语义事件）。
type TaskEvent string

const (
	EventClaim           TaskEvent = "claim"            // Agent 领取成功（claim 端点）
	EventFirstStatus     TaskEvent = "first_status"     // 首条 type=STATUS 事件（events 端点）
	EventCancelTriggered TaskEvent = "cancel_triggered" // DISPATCHED 置取消标志后首条 STATUS（v1.0.5 I-2）
	EventResultCompleted TaskEvent = "result_completed" // result(COMPLETED)
	EventResultFailed    TaskEvent = "result_failed"    // result(FAILED)
	EventResultCancelled TaskEvent = "result_cancelled" // result(CANCELLED)
	EventUserCancel      TaskEvent = "user_cancel"      // 用户取消（前端→Go，day41 REST）
	EventLeaseExpired    TaskEvent = "lease_expired"    // 租约过期（day37 清扫器）
	EventCleanupAck      TaskEvent = "cleanup_ack"      // 清理确认（day43 端点）
)

// TransitionAction 转移动作枚举（协议表 6 动作列语义）。
type TransitionAction string

const (
	ActionCreateExecution TransitionAction = "create_execution" // 创建 test_execution 并写租约（行 1）
	ActionBroadcastStart  TransitionAction = "broadcast_start"  // SSE 广播执行开始（行 2）
	ActionBroadcastCancel TransitionAction = "broadcast_cancel" // 取消指令经心跳响应下发（行 5）
	ActionBroadcastFinal  TransitionAction = "broadcast_final"  // 终态广播（行 3/6）
	ActionRecordResult    TransitionAction = "record_result"    // 结果落库（行 3/7'）
	ActionRecordFailure   TransitionAction = "record_failure"   // 记录失败原因，保留已有证据（行 4）
	ActionSetCancelFlag   TransitionAction = "set_cancel_flag"  // DISPATCHED 置 Redis 取消标志（v1.0.5 I-2）
	ActionNoCleanup       TransitionAction = "no_cleanup"       // PENDING 直接取消（无执行清理负担，v1.0.5）
	ActionMarkLeaseLost   TransitionAction = "mark_lease_lost"  // 标记 lease_lost（行 10）
	ActionQueueCleanup    TransitionAction = "queue_cleanup"    // 仅排队清理任务，破坏性任务不重跑（行 10）
	ActionRecordOnly      TransitionAction = "record_only"      // 记录不改态（v1.0.4 ⑦ 兜底）
	ActionGenerateReport  TransitionAction = "generate_report"  // 生成报告（表 6 行 3 动作列第三件事）
)

// TransitionResult 一次转移判定：To="" 且 OK=true 表示无状态转移但有动作（动作型事件）；
// OK=false 表示拒绝（终态兜底——端点层"记录+2xx+不改状态"吞掉）。
type TransitionResult struct {
	To      TaskState
	Actions []TransitionAction
	OK      bool
}

// transitions 显式全表（63 格；缺格零值=拒绝——与终态空行同语义，矩阵测试钉全量）。
var transitions = map[TaskState]map[TaskEvent]TransitionResult{
	StatePending: {
		EventClaim:      {To: StateDispatched, OK: true, Actions: []TransitionAction{ActionCreateExecution}},
		EventUserCancel: {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionNoCleanup}},
		EventCleanupAck: {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
	},
	StateDispatched: {
		EventFirstStatus:     {To: StateRunning, OK: true, Actions: []TransitionAction{ActionBroadcastStart}},
		EventCancelTriggered: {To: StateCancelling, OK: true, Actions: []TransitionAction{ActionBroadcastCancel}},
		EventResultCompleted: {To: StateCompleted, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionGenerateReport, ActionBroadcastFinal}},
		EventResultFailed:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionRecordFailure}},
		EventResultCancelled: {To: "", OK: true, Actions: []TransitionAction{ActionRecordResult}},
		EventUserCancel:      {To: "", OK: true, Actions: []TransitionAction{ActionSetCancelFlag}}, // v1.0.5 I-2：置标志，首条 STATUS 才转
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},
		EventCleanupAck:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
	},
	StateRunning: {
		EventResultCompleted: {To: StateCompleted, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionGenerateReport, ActionBroadcastFinal}},
		EventResultFailed:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionRecordFailure}},
		EventResultCancelled: {To: "", OK: true, Actions: []TransitionAction{ActionRecordResult}},
		EventUserCancel:      {To: StateCancelling, OK: true, Actions: []TransitionAction{ActionBroadcastCancel}},
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},
		EventCleanupAck:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
	},
	StateCancelling: {
		// result_* 三格均"视同 cleanup-ack 转移"（行 7'/行 9）——含取消终态广播（day36 评审 I-1）
		EventCleanupAck:      {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionBroadcastFinal}},
		EventResultCancelled: {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionBroadcastFinal}},
		EventResultCompleted: {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionBroadcastFinal}},
		EventResultFailed:    {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordFailure, ActionBroadcastFinal}},
		EventUserCancel:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},
	},
	// 终态：空行=全事件拒绝（零值 OK=false）。
	StateCompleted: {}, StateFailed: {}, StateCancelled: {},
}

// transition 查一次转移判定（唯一事实源；缺格=拒绝）。
func transition(from TaskState, event TaskEvent) TransitionResult {
	return transitions[from][event]
}
