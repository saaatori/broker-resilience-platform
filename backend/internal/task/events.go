// events.go 执行事件接收（M2 第 34 天，协议 v1.0.5 §2.4）：
// 归属 404 → schema 三型 + ts 校验 → Redis SETNX 幂等（重复静默）→ 终态兜底 →
// SSE 即时转发（不持久化）→ STATUS 时 CAS 状态转移（取消标志优先）+ 子状态更新。
// 架构护栏：事件本体永不入 PG（决策 6）——PG 只承状态转移与子状态两类元数据写。
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// ErrExecutionNotFound 执行不存在或非本 Agent 名下（§3：非本 Agent 视同不存在——同一 404 报文）。
var ErrExecutionNotFound = errors.New("execution not found")

// ErrInvalidEvent 事件体不合法（schema 三型错配 / ts 非 RFC3339 带时区）。
var ErrInvalidEvent = errors.New("invalid event")

// 任务终态集（协议 §4）。
var taskFinalStates = map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELLED": true}

// eventEnvelope 事件请求体的解码形态（校验后取 seq/ts/type/payload）。
type eventEnvelope struct {
	Seq     int64           `json:"seq"`
	Ts      string          `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// sseEventName 协议 type → SSE 事件名。
func sseEventName(eventType string) string {
	switch eventType {
	case "STATUS":
		return "status"
	case "LOG":
		return "log"
	case "PROGRESS":
		return "progress"
	}
	return eventType
}

// Event 处理一条执行事件上报。返回 nil=202（含重复静默与终态记录）；
// ErrExecutionNotFound→404；ErrInvalidEvent→400；闸门故障→包装错误（500）。
func (s *Service) Event(agent *model.AgentInstance, eid string, raw []byte) error {
	// 归属（§3 v1.0.5：三 /executions/* 端点限本 Agent 名下——非本 Agent 视同不存在）
	exec, err := s.repo.FindExecutionByEID(eid)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrExecutionNotFound
	}
	if err != nil {
		return fmt.Errorf("find execution: %w", err)
	}
	if exec.AgentID != agent.ID {
		return ErrExecutionNotFound
	}

	// schema 三型 + ts（v6 format no-op → Go 原生 RFC3339 后置，等价落地）
	if err := s.deps.Events.Validate(raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	var ev eventEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil { // Validate 已保证可解，防御分支
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	if !ValidTimestamp(ev.Ts) {
		return fmt.Errorf("%w: ts 需为 RFC3339 带时区（UTC ISO 8601，决策 11）", ErrInvalidEvent)
	}

	// 幂等闸门：SETNX (eid, seq)——false=重复回传，静默 202（不转发不转移）
	fresh, err := s.deps.Gate.TryClaimSeq(eid, ev.Seq)
	if err != nil {
		return fmt.Errorf("event gate: %w", err) // Redis 故障 5xx，不降级放行（§2.4 v1.0.4/05）
	}
	if !fresh {
		return nil
	}

	// 终态兜底（§4 v1.0.1）：记录、202、不改状态（子状态也不覆盖——终态语义已收口）
	task, err := s.repo.FindTaskByID(exec.TaskID)
	if err != nil {
		return fmt.Errorf("find task: %w", err)
	}
	if taskFinalStates[task.Status] {
		log.Printf("task: 终态任务收到迟到事件（task=%d eid=%s seq=%d），记录不改态", task.ID, eid, ev.Seq)
		return nil
	}

	// SSE 即时转发（透传原事件 + execution_id；按任务维度路由）
	s.publishEvent(exec.TaskID, eid, raw, ev.Type)

	// STATUS：CAS 转移（取消标志优先，v1.0.5 I-2）+ 子状态更新（表 20）
	if ev.Type == "STATUS" {
		var payload struct {
			Substate string `json:"substate"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return fmt.Errorf("%w: substate 解码失败: %v", ErrInvalidEvent, err)
		}
		cancel, cErr := s.deps.Gate.CancelFlag(eid)
		if cErr != nil {
			// 读取故障按无标志处理（转 RUNNING）——取消闭环有心跳 cancel_requested 兜底，记档
			log.Printf("task: exec:cancel 读取失败（按无标志处理）: %v", cErr)
		}
		// 转移经状态机决策表（唯一事实源）：置标志=EventCancelTriggered（v1.0.5 I-2），否则 EventFirstStatus
		ev := EventFirstStatus
		if cancel {
			ev = EventCancelTriggered
		}
		if tj := transition(TaskState(task.Status), ev); tj.OK && tj.To != "" {
			// 转移仅 DISPATCHED 合法（决策表）；RUNNING/终态收 STATUS=拒绝（无转移）但子状态仍更新（表 20）
			moved, err := s.repo.TransitionTaskStatus(task.ID, task.Status, string(tj.To))
			if err != nil {
				return fmt.Errorf("transition task: %w", err)
			}
			if moved { // ActionBroadcastStart（决策表动作）
				s.deps.Hub.Publish(task.ID, SSEEvent{Name: "task", Data: map[string]any{"task_id": task.ID, "status": string(tj.To)}})
			}
		}
		if err := s.repo.UpdateExecSubstate(eid, payload.Substate); err != nil {
			return fmt.Errorf("update exec substate: %w", err)
		}
	}
	return nil
}

// publishEvent 透传广播：Agent 事件原样 + execution_id（前端按 seq 重排并去重，§2.4 v1.0.5）。
func (s *Service) publishEvent(taskID int64, eid string, raw []byte, eventType string) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return // Validate 已保证可解，不可达
	}
	m, _ := payload.(map[string]any)
	if m != nil {
		m["execution_id"] = eid
	}
	s.deps.Hub.Publish(taskID, SSEEvent{Name: sseEventName(eventType), Data: m})
}
