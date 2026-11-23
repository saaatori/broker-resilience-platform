// sweeper.go 租约清扫（M2 第 37 天，协议 v1.0.5 §4 表行 10 的触发器）：
// 周期扫描租约过期的执行——任务经决策表 lease_expired 行 CAS→FAILED（破坏性任务不自动重跑，
// 只排队清理）；exec 非终态置 ABORTED/lease_lost/finished_at（day33 M-2：终态后心跳不再刷新）；
// eid 入 Redis cleanup:queue（day43 消费）。中断窗口（exec DONE+任务非终态+过期，day35 记档）
// 兜底扫：任务收口 FAILED 但 exec 保持 DONE（result 语义保全）。
package task

import (
	"fmt"
	"log"
	"time"
)

// leaseLostReason 表 20 exit_reason 枚举值。
const leaseLostReason = "lease_lost"

// SweepOnce 执行一轮扫描清扫，返回清扫条数。逐条动作序：
// 决策表判定 → CAS 转移（from=扫描快照态；不中=并发已被转，跳过且不入队）
// → exec 非终态置 ABORTED/lease_lost/finished_at → SSE 终态广播 → 队列 RPUSH（失败告警不阻断）。
func (s *Service) SweepOnce(now time.Time) (int, error) {
	views, err := s.repo.FindLeaseExpired(now)
	if err != nil {
		return 0, fmt.Errorf("find lease expired: %w", err)
	}
	swept := 0
	for _, v := range views {
		tj := transition(TaskState(v.TaskStatus), EventLeaseExpired)
		if !tj.OK || tj.To == "" {
			continue // 扫描集与决策表不一致的防御（repo 过滤已保证三态）
		}
		moved, err := s.repo.TransitionTaskStatus(v.Exec.TaskID, v.TaskStatus, string(tj.To))
		if err != nil {
			log.Printf("task: 清扫转移失败（task=%d）: %v", v.Exec.TaskID, err)
			continue
		}
		if !moved {
			continue // 并发已被转（result 抢先）——不入队不重复
		}
		// exec 非终态才收口（DONE 的保持——result 语义保全）
		if v.Exec.Status != string(execStatusDone) && v.Exec.Status != string(execStatusAborted) {
			if err := s.repo.FinishExecution(v.Exec.ExecutionID, string(execStatusAborted), leaseLostReason); err != nil {
				log.Printf("task: 清扫收口执行失败（eid=%s）: %v", v.Exec.ExecutionID, err)
			}
		}
		// ActionBroadcastFinal（决策表动作）
		s.deps.Hub.Publish(v.Exec.TaskID, SSEEvent{Name: "task",
			Data: map[string]any{"task_id": v.Exec.TaskID, "status": string(tj.To)}})
		// ActionQueueCleanup（决策表动作）
		if s.deps.CleanupQueue != nil {
			if err := s.deps.CleanupQueue.Enqueue(v.Exec.ExecutionID); err != nil {
				log.Printf("task: 清理队列入队失败（eid=%s，不阻断状态收口）: %v", v.Exec.ExecutionID, err)
			}
		}
		swept++
	}
	return swept, nil
}

// Sweeper 周期清扫器（单 goroutine；单副本部署无重入面——多 Agent 横向扩展已 CUT）。
type Sweeper struct {
	svc      *Service
	interval time.Duration
}

// NewSweeper 构造（interval 默认 45s 由 main 装配决定；BRP_SWEEP_INTERVAL 可配）。
// interval<=0 钳到 45s——time.NewTicker 非正数会 panic（day37 评审 M-5 纵深防御）。
func NewSweeper(svc *Service, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = 45 * time.Second
	}
	return &Sweeper{svc: svc, interval: interval}
}

// Loop 阻塞循环（main 以 go 起）；单轮失败记日志继续（清扫是自愈型后台任务）。
func (w *Sweeper) Loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	log.Printf("task: 租约清扫器启动（周期 %s，口径 45s 租约）", w.interval)
	for range ticker.C {
		if _, err := w.svc.SweepOnce(time.Now()); err != nil {
			log.Printf("task: 清扫轮失败（下一轮重试）: %v", err)
		}
	}
}
