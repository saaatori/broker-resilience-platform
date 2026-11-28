// cancel.go 用户取消入口（M2 第 41 天，协议 v1.0.5 I-2：前端→Go 的 REST，非 Agent 六端点）。
// 分派全部经决策表 EventUserCancel 行（唯一事实源）：PENDING 直接收口 CANCELLED（无执行清理负担）；
// DISPATCHED 仅置 Redis 取消标志（首条 STATUS 到达即转 CANCELLING——events.go day34 已实现）；
// RUNNING 转 CANCELLING（取消指令随心跳响应下发，judgeExec day33）；CANCELLING 幂等；终态冲突。
// 并发复核（day41 评审 I-1/I-2）：CAS 不中或置标志后都重读任务态再分派——
// 取消与 claim/首条 STATUS/result 的交错下不返回假状态、不静默丢取消。
package task

import (
	"errors"
	"fmt"
	"log"
)

// ErrCancelConflict 终态任务不可取消（handler 409）。
var ErrCancelConflict = errors.New("task already in terminal state")

// cancelReruns 读-动作交错的最大重试轮数（每轮要么完成要么状态被并发方推进；
// 3 轮覆盖 claim→STATUS→result 全链抢先，仍不收敛则按当前态分派收口）。
const cancelReruns = 3

// Cancel 处理一次用户取消，返回动作后的任务状态（响应用）。
func (s *Service) Cancel(taskID int64) (string, error) {
	for round := 0; round < cancelReruns; round++ {
		task, err := s.repo.FindTaskByID(taskID)
		if err != nil {
			return "", err // gorm.ErrRecordNotFound → handler 404；其余 500
		}

		tj := transition(TaskState(task.Status), EventUserCancel)
		if !tj.OK {
			return "", fmt.Errorf("%w: %s", ErrCancelConflict, task.Status)
		}

		switch TaskState(task.Status) {
		case StatePending:
			moved, err := s.repo.TransitionTaskStatus(taskID, task.Status, string(StateCancelled))
			if err != nil {
				return "", fmt.Errorf("cancel transition: %w", err)
			}
			if moved {
				s.deps.Hub.Publish(taskID, SSEEvent{Name: "task",
					Data: map[string]any{"task_id": taskID, "status": string(StateCancelled)}})
				return string(StateCancelled), nil
			}
			continue // 被 claim（或另一取消）抢先：重读分派

		case StateDispatched:
			// v1.0.5 I-2：只置标志，状态不动——首条 STATUS 到达时经 EventCancelTriggered 转 CANCELLING。
			exec, err := s.repo.FindActiveExecutionByTaskID(taskID)
			if err != nil {
				return "", fmt.Errorf("find active execution: %w", err)
			}
			if err := s.deps.Gate.SetCancelFlag(exec.ExecutionID); err != nil {
				// 标志写不进=取消指令送不到（心跳只认任务态 CANCELLING）——按 500 上抛，不静默假成功
				return "", fmt.Errorf("set cancel flag: %w", err)
			}
			// 复查（评审 I-2）：置标志窗口内首条 STATUS 可能已把任务转 RUNNING——
			// 标志此后无人读（仅首条 STATUS 读），取消会静默丢失；补 CAS 转 CANCELLING 等价 RUNNING 分支。
			fresh, err := s.repo.FindTaskByID(taskID)
			if err != nil {
				return "", fmt.Errorf("recheck task: %w", err)
			}
			switch TaskState(fresh.Status) {
			case StateDispatched:
				return fresh.Status, nil // 标志已就位，首条 STATUS 到达即转
			case StateRunning:
				moved, err := s.repo.TransitionTaskStatus(taskID, fresh.Status, string(StateCancelling))
				if err != nil {
					return "", fmt.Errorf("cancel transition: %w", err)
				}
				if moved {
					s.deps.Hub.Publish(taskID, SSEEvent{Name: "task",
						Data: map[string]any{"task_id": taskID, "status": string(StateCancelling)}})
					return string(StateCancelling), nil
				}
				continue // 又被抢先（result/清扫）：重读分派
			case StateCancelling:
				return fresh.Status, nil // 首条 STATUS 已吃到标志转 CANCELLING——闭环成立
			default: // 终态：执行已收口，标志成死键由 TTL 回收
				return "", fmt.Errorf("%w: %s", ErrCancelConflict, fresh.Status)
			}

		case StateRunning:
			moved, err := s.repo.TransitionTaskStatus(taskID, task.Status, string(StateCancelling))
			if err != nil {
				return "", fmt.Errorf("cancel transition: %w", err)
			}
			if moved {
				// judgeExec 走任务态 CANCELLING 判 cancel_requested——无需 Redis 标志（DISPATCHED 专用）
				s.deps.Hub.Publish(taskID, SSEEvent{Name: "task",
					Data: map[string]any{"task_id": taskID, "status": string(StateCancelling)}})
				return string(StateCancelling), nil
			}
			continue // 被 result（I-4 照录收口）或清扫抢先：重读后按新态分派（终态 → 409）

		case StateCancelling:
			return task.Status, nil // 决策表 RecordOnly：重复取消幂等

		default:
			// 决策表已由 OK 分支兜底（终态空行拒绝）；防御：不可达
			log.Printf("task: cancel 到达未覆盖状态 %q（task=%d）", task.Status, taskID)
			return "", fmt.Errorf("%w: %s", ErrCancelConflict, task.Status)
		}
	}
	// 多轮交错后仍不收敛（理论不可达）：末次读到的态要么已分派要么按冲突收口
	last, err := s.repo.FindTaskByID(taskID)
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w: %s", ErrCancelConflict, last.Status)
}
