package task

import (
	"fmt"
	"testing"
)

// matrix 63 格期望表（7 状态 × 9 事件）——逐格对照协议 v1.0.5 §4 转移表（10 行原文）
// 与 v1.0.1 ※/v1.0.4 ※※/v1.0.5 I-1/I-2/终态兜底。To="" 表示无状态转移（ok 仍可为 true——动作型事件）。
var matrix = map[TaskState]map[TaskEvent]TransitionResult{
	StatePending: {
		EventClaim:           {To: StateDispatched, OK: true, Actions: []TransitionAction{ActionCreateExecution}}, // 表行 1
		EventUserCancel:      {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionNoCleanup}},        // v1.0.5 I-2：直接置 CANCELLED（无执行清理负担）
		EventCleanupAck:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},                   // v1.0.4 ⑦：记录不改态
		EventFirstStatus:     {OK: false},
		EventCancelTriggered: {OK: false},
		EventResultCompleted: {OK: false},
		EventResultFailed:    {OK: false},
		EventResultCancelled: {OK: false},
		EventLeaseExpired:    {OK: false}, // PENDING 无租约
	},
	StateDispatched: {
		EventFirstStatus:     {To: StateRunning, OK: true, Actions: []TransitionAction{ActionBroadcastStart}},                                             // 表行 2（v1.0.1：首条 STATUS 不论子状态）
		EventCancelTriggered: {To: StateCancelling, OK: true, Actions: []TransitionAction{ActionBroadcastCancel}},                                         // v1.0.5 I-2：置标志后首条 STATUS 即转 CANCELLING
		EventResultCompleted: {To: StateCompleted, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionGenerateReport, ActionBroadcastFinal}}, // 表行 8 ※※（STATUS 前崩溃恢复直传）
		EventResultFailed:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionRecordFailure}},                                               // 表行 8 ※※
		EventResultCancelled: {To: "", OK: true, Actions: []TransitionAction{ActionRecordResult}},                                                         // v1.0.5 I-1：照录不改态（清扫收口 lease_lost）
		EventUserCancel:      {To: "", OK: true, Actions: []TransitionAction{ActionSetCancelFlag}},                                                        // v1.0.5 I-2：置标志，首条 STATUS 才转
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},                           // 表行 7
		EventCleanupAck:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
		EventClaim:           {OK: false},
	},
	StateRunning: {
		EventResultCompleted: {To: StateCompleted, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionGenerateReport, ActionBroadcastFinal}}, // 表行 3
		EventResultFailed:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionRecordFailure}},                                               // 表行 4
		EventResultCancelled: {To: "", OK: true, Actions: []TransitionAction{ActionRecordResult}},                                                         // v1.0.5 I-1
		EventUserCancel:      {To: StateCancelling, OK: true, Actions: []TransitionAction{ActionBroadcastCancel}},                                         // 表行 5
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},                           // 表行 7
		EventCleanupAck:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},
		EventClaim:           {OK: false},
		EventFirstStatus:     {OK: false}, // 已 RUNNING：后续 STATUS 不再触发
		EventCancelTriggered: {OK: false}, // cancel_triggered 仅 DISPATCHED 语义
	},
	StateCancelling: {
		EventCleanupAck:      {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionBroadcastFinal}},                      // 表行 6
		EventResultCancelled: {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionBroadcastFinal}},  // 表行 7' ※：视同 cleanup-ack（含广播）
		EventResultCompleted: {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordResult, ActionBroadcastFinal}},  // 表行 9 ※※：取消优先，result 照录
		EventResultFailed:    {To: StateCancelled, OK: true, Actions: []TransitionAction{ActionRecordFailure, ActionBroadcastFinal}}, // 表行 9 ※※
		EventUserCancel:      {To: "", OK: true, Actions: []TransitionAction{ActionRecordOnly}},                                      // 幂等：已在取消中
		EventLeaseExpired:    {To: StateFailed, OK: true, Actions: []TransitionAction{ActionMarkLeaseLost, ActionQueueCleanup}},      // v1.0.1 ②：CANCELLING 并入租约过期
		EventClaim:           {OK: false},
		EventFirstStatus:     {OK: false},
		EventCancelTriggered: {OK: false},
	},
	// 终态 × 全事件 = 拒绝（协议终态兜底：记录+2xx+不改状态——状态机层面 ok=false；端点吞为幂等响应）
	StateCompleted: {}, StateFailed: {}, StateCancelled: {},
}

// TestStateMachineFullMatrix 63 格全矩阵（双层循环生成杜绝漏格；期望表缺格即红）。
func TestStateMachineFullMatrix(t *testing.T) {
	states := []TaskState{StatePending, StateDispatched, StateRunning, StateCancelling, StateCompleted, StateFailed, StateCancelled}
	events := []TaskEvent{EventClaim, EventFirstStatus, EventCancelTriggered, EventResultCompleted, EventResultFailed,
		EventResultCancelled, EventUserCancel, EventLeaseExpired, EventCleanupAck}

	count := 0
	for _, s := range states {
		for _, e := range events {
			count++
			t.Run(fmt.Sprintf("%s×%s", s, e), func(t *testing.T) {
				row, found := matrix[s]
				if !found {
					t.Fatalf("期望表缺状态行 %s", s)
				}
				exp, found := row[e]
				if !found {
					if s == StateCompleted || s == StateFailed || s == StateCancelled {
						exp = TransitionResult{} // 终态空行=全拒绝（设计如此，见下自检）
					} else {
						t.Fatalf("期望表缺格 %s×%s", s, e)
					}
				}
				got := transition(s, e)
				if got.OK != exp.OK {
					t.Fatalf("ok: want %v got %v", exp.OK, got.OK)
				}
				if got.To != exp.To {
					t.Fatalf("to: want %q got %q", exp.To, got.To)
				}
				if fmt.Sprint(got.Actions) != fmt.Sprint(exp.Actions) {
					t.Fatalf("actions: want %v got %v", exp.Actions, got.Actions)
				}
			})
		}
	}
	if count != 63 {
		t.Fatalf("矩阵应为 63 格（7×9），实际 %d", count)
	}
	// 终态行全部显式拒绝（map 空行=零值 OK false——语义即拒绝；此处自检终态行非空格子数）
	for _, fin := range []TaskState{StateCompleted, StateFailed, StateCancelled} {
		if len(matrix[fin]) != 0 {
			t.Fatalf("终态 %s 期望行应为空（全拒绝）", fin)
		}
	}
}

// TestProtocolTableRows 协议 §4 转移表 10 行原文专项（行号注释=文档锚点，文档升版此处先红）。
func TestProtocolTableRows(t *testing.T) {
	cases := []struct {
		row  string // 协议行标识
		from TaskState
		ev   TaskEvent
		to   TaskState
	}{
		{"行1 PENDING+领取→DISPATCHED", StatePending, EventClaim, StateDispatched},
		{"行2 DISPATCHED+首条STATUS→RUNNING", StateDispatched, EventFirstStatus, StateRunning},
		{"行3 RUNNING+result(COMPLETED)→COMPLETED", StateRunning, EventResultCompleted, StateCompleted},
		{"行4 RUNNING+result(FAILED)→FAILED", StateRunning, EventResultFailed, StateFailed},
		{"行5 RUNNING+用户取消→CANCELLING", StateRunning, EventUserCancel, StateCancelling},
		{"行6 CANCELLING+cleanup-ack→CANCELLED", StateCancelling, EventCleanupAck, StateCancelled},
		{"行7' CANCELLING+result(CANCELLED)→CANCELLED", StateCancelling, EventResultCancelled, StateCancelled},
		{"行8 DISPATCHED+result(COMPLETED/FAILED)→终态", StateDispatched, EventResultCompleted, StateCompleted},
		{"行9 CANCELLING+result(COMPLETED/FAILED)→CANCELLED", StateCancelling, EventResultCompleted, StateCancelled},
		{"行10 三态+租约过期→FAILED", StateRunning, EventLeaseExpired, StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			got := transition(tc.from, tc.ev)
			if !got.OK || got.To != tc.to {
				t.Fatalf("want %s got %+v", tc.to, got)
			}
		})
	}
}

// TestLeaseLostScope lease_lost 分支域（验收原文点名的分支）：恰 DISPATCHED/RUNNING/CANCELLING 三态入，
// PENDING（无租约）与终态（已收口）拒绝。
func TestLeaseLostScope(t *testing.T) {
	for _, s := range []TaskState{StateDispatched, StateRunning, StateCancelling} {
		got := transition(s, EventLeaseExpired)
		if !got.OK || got.To != StateFailed {
			t.Errorf("%s×lease_expired 应 FAILED(lease_lost): %+v", s, got)
		}
		if !containsAction(got.Actions, ActionMarkLeaseLost) || !containsAction(got.Actions, ActionQueueCleanup) {
			t.Errorf("%s×lease_expired 应含 mark_lease_lost+queue_cleanup: %v", s, got.Actions)
		}
	}
	for _, s := range []TaskState{StatePending, StateCompleted, StateFailed, StateCancelled} {
		if got := transition(s, EventLeaseExpired); got.OK {
			t.Errorf("%s×lease_expired 应拒绝: %+v", s, got)
		}
	}
}

func containsAction(actions []TransitionAction, want TransitionAction) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}
