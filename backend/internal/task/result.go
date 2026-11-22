// result.go 结果回传（M2 第 35 天，协议 v1.0.5 §2.5/§4）：
// first-wins 幂等（PG uq，重复/尾随 200 直返零副作用）→ 状态机转移矩阵（含 v1.0.4 ※※
// 与 v1.0.5 I-1/I-4 全分支）→ 三表落库 → exec 子状态终态（day33 M-2 强制项）→ 取消键清理 → 终态广播。
// 幂等分层对照记档：events=Redis TTL 窗口去重（进度级，at-most-once 窗口已记档）；
// result=PG first-wins（账务级，无窗口——迟到/重复由 uq 拦截，落库失败不占幂等位）。
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// ErrInvalidResult 结果回传体不合法（schema 键集 / attempt 不匹配）。
var ErrInvalidResult = errors.New("invalid result")

// ErrResultExists 并发窗口的迟到方（uq 冲突）——内部哨兵，对 Agent 表现为幂等 200。
var ErrResultExists = errors.New("result already exists")

// resultEnvelope 回传体解码形态。
type resultEnvelope struct {
	Attempt        int             `json:"attempt"`
	Status         string          `json:"status"`
	Summary        json.RawMessage `json:"summary"`
	Reconciliation json.RawMessage `json:"reconciliation"`
	Artifacts      []string        `json:"artifacts"`
}

// decideTransition 状态机转移矩阵（协议 §4 转移表全积，15 格决策表测试钉死）：
// 返回目标态；""=不改态（取消优先/非取消态 CANCELLED/终态兜底/PENDING 防御）。
func decideTransition(taskStatus, resultStatus string) string {
	switch {
	case resultStatus == "COMPLETED" || resultStatus == "FAILED":
		switch taskStatus {
		case "RUNNING", "DISPATCHED": // DISPATCHED=v1.0.4 ※※（STATUS 前崩溃恢复直传）
			return resultStatus
		case "CANCELLING": // v1.0.4 ※※ 表行 9：取消意图优先——result 照录、任务转取消终态（清理照常兜底）
			return "CANCELLED"
		}
	case resultStatus == "CANCELLED" && taskStatus == "CANCELLING": // v1.0.1 ※：视同 cleanup-ack
		return "CANCELLED"
	}
	// 其余（终态兜底 / PENDING 防御 / 非取消态 CANCELLED=v1.0.5 I-1 照录）均不改态。
	return ""
}

// execTerminalSubstate 执行子状态终态映射（day33 M-2 强制项；裁决记档）：
// COMPLETED/FAILED→DONE（失败也是流程走完）、CANCELLED→ABORTED（中止）。
func execTerminalSubstate(resultStatus string) string {
	if resultStatus == "CANCELLED" {
		return execStatusAborted
	}
	return execStatusDone
}

// exitReasonOf 表 20 退出原因映射：FAILED=error / CANCELLED=cancelled / COMPLETED 空。
func exitReasonOf(resultStatus string) string {
	switch resultStatus {
	case "FAILED":
		return "error"
	case "CANCELLED":
		return "cancelled"
	}
	return ""
}

// metricUnits 五指标单位映射（表 23：%/s）。
var metricUnits = map[string]string{
	"processed_rate": "%", "dup_rate": "%", "loss_rate": "%",
	"avg_delivery_latency_s": "s", "recovery_time_s": "s",
}

// summaryVerdict 从 summary 提取 verdict（test_result.verdict，表 22）。
func summaryVerdict(summary json.RawMessage) string {
	var s struct {
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal(summary, &s) // schema 已保证键存在
	return s.Verdict
}

// summaryMetrics 提取五指标数值。宽松解码 map[string]any（summary 含 verdict/details 等非数值键，
// 严格 map[string]json.Number 会整包失败）——逐键 float64 断言；null/缺失自然跳过（null=场景不适用）。
func summaryMetrics(summary json.RawMessage) map[string]float64 {
	var m map[string]any
	if err := json.Unmarshal(summary, &m); err != nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(metricUnits))
	for key := range metricUnits {
		if f, ok := m[key].(float64); ok {
			out[key] = f
		}
	}
	return out
}

// Result 处理一次结果回传。nil=200（含 first-wins 幂等命中与并发迟到方）；
// ErrExecutionNotFound→404；ErrInvalidResult→400；其余→500。
func (s *Service) Result(agent *model.AgentInstance, eid string, raw []byte) error {
	exec, err := s.repo.FindExecutionByEID(eid)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrExecutionNotFound
	}
	if err != nil {
		return fmt.Errorf("find execution: %w", err)
	}
	if exec.AgentID != agent.ID {
		return ErrExecutionNotFound // §3：非本 Agent 视同不存在
	}

	if err := s.deps.Result.Validate(raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	var req resultEnvelope
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	if req.Attempt != exec.Attempt {
		return fmt.Errorf("%w: attempt %d 与执行轮次 %d 不匹配", ErrInvalidResult, req.Attempt, exec.Attempt)
	}

	// first-wins：已有结果=重复/尾随回传——200 直返零副作用（v1.0.5 I-4：尾随 CANCELLED 被忽略）。
	// 不做"自愈转移"：表 22 无 result status 列，重放 status 不可信（异 status 尾随会把
	// I-1 照录场景误转移）；首落在转移前中断的窗口由 day37 租约清扫收口（DISPATCHED/RUNNING/
	// CANCELLING 均有 FAILED(lease_lost) 兜底行）——裁决记档。
	if _, err := s.repo.FindResultByEID(eid); err == nil {
		log.Printf("task: result 重复/尾随回传（eid=%s attempt=%d），first-wins 忽略", eid, req.Attempt)
		// 幂等重放也兜底清取消键（执行已终态，键残留属异常但无害清理；day35 评审 I-5 实证路径）
		if err := s.deps.Gate.RemoveCancelFlag(eid); err != nil {
			log.Printf("task: exec:cancel 清理失败（不阻断）: %v", err)
		}
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("find result: %w", err)
	}

	// 三表落库：先 result（uq=幂等权威），首落成功才写 summary/report——
	// 中途失败窗口不自愈（见上），任务态由 day37 清扫兜底；summary/report 无重复风险
	//（重放被 first-wins 拦截，到不了这里）。
	r := &model.TestResult{
		ExecutionID:    eid,
		Verdict:        summaryVerdict(req.Summary),
		Metrics:        string(req.Summary),
		Reconciliation: string(req.Reconciliation),
	}
	if err := s.repo.CreateResult(r); err != nil {
		if errors.Is(err, ErrResultExists) {
			return nil // 并发迟到方：幂等语义 200
		}
		return fmt.Errorf("create result: %w", err)
	}
	if metrics := summaryMetrics(req.Summary); len(metrics) > 0 {
		if err := s.repo.SaveMetricSummaries(eid, metrics, metricUnits); err != nil {
			return fmt.Errorf("save metric summaries: %w", err)
		}
	}
	if len(req.Artifacts) > 0 {
		if err := s.repo.SaveReportArtifacts(exec.TaskID, eid, req.Artifacts); err != nil {
			return fmt.Errorf("save report artifacts: %w", err)
		}
	}

	s.finalizeTransition(exec.TaskID, req.Status)
	if err := s.repo.FinishExecution(eid, execTerminalSubstate(req.Status), exitReasonOf(req.Status)); err != nil {
		return fmt.Errorf("finish execution: %w", err)
	}
	if err := s.deps.Gate.RemoveCancelFlag(eid); err != nil {
		log.Printf("task: exec:cancel 清理失败（不阻断，键随执行结束语义过期）: %v", err)
	}
	return nil
}

// finalizeTransition 查任务当前态 → 决策表 → CAS（from=查询快照；并发已改态则不命中不广播，幂等安全）。
func (s *Service) finalizeTransition(taskID int64, resultStatus string) {
	task, err := s.repo.FindTaskByID(taskID)
	if err != nil {
		log.Printf("task: result 转移前查任务失败（task=%d）: %v", taskID, err)
		return
	}
	to := decideTransition(task.Status, resultStatus)
	if to == "" {
		return
	}
	moved, err := s.repo.TransitionTaskStatus(task.ID, task.Status, to)
	if err != nil {
		log.Printf("task: result 状态转移失败（task=%d → %s）: %v", taskID, to, err)
		return
	}
	if moved {
		s.deps.Hub.Publish(task.ID, SSEEvent{Name: "task", Data: map[string]any{"task_id": task.ID, "status": to}})
	}
}
