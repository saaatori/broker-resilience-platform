// result.go 结果回传（M2 第 35 天，协议 v1.0.5 §2.5/§4；v1.1 增 failure / faults）：
// first-wins 幂等（PG uq，重复/尾随 200 直返零副作用）→ 状态机转移矩阵（含 v1.0.4 ※※
// 与 v1.0.5 I-1/I-4 全分支）→ 三表落库 → exec 子状态终态（day33 M-2 强制项）→ 取消键清理 → 终态广播。
// 幂等分层对照记档：events=Redis TTL 窗口去重（进度级，at-most-once 窗口已记档）；
// result=PG first-wins（账务级，无窗口——迟到/重复由 uq 拦截，落库失败不占幂等位）。
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"time"
	"unicode/utf8"

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
	Failure        json.RawMessage `json:"failure"` // 协议 v1.1：失败原因，仅 FAILED 携带（schema 约束）
	Faults         json.RawMessage `json:"faults"`  // 协议 v1.1：故障时间线——当前仅校验，落库随 day53（fault_injection 表）
	Summary        json.RawMessage `json:"summary"`
	Reconciliation json.RawMessage `json:"reconciliation"`
	Artifacts      []string        `json:"artifacts"`
}

// decideTransition result 面转移（statemachine 决策表的薄包装——15 格测试钉 result 面语义；
// 唯一事实源见 statemachine.go，本函数仅为既有调用方保持 string 签名）。
func decideTransition(taskStatus, resultStatus string) string {
	return string(transition(TaskState(taskStatus), resultEventOf(resultStatus)).To)
}

// resultEventOf result status → 状态机事件。
func resultEventOf(resultStatus string) TaskEvent {
	switch resultStatus {
	case "COMPLETED":
		return EventResultCompleted
	case "FAILED":
		return EventResultFailed
	case "CANCELLED":
		return EventResultCancelled
	}
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

// exitReasonOf 表 20 退出原因映射：FAILED→failure.reason（协议 v1.1；未携带时兜底 error——
// 不因为缺原因把整份结果拒之门外）/ CANCELLED=cancelled / COMPLETED 空。
func exitReasonOf(resultStatus string, failure json.RawMessage) string {
	switch resultStatus {
	case "FAILED":
		if reason := failureReason(failure); reason != "" {
			return reason
		}
		return "error"
	case "CANCELLED":
		return "cancelled"
	}
	return ""
}

// failureReason 取 failure.reason；未携带 failure 块（或解码失败）返回空串。
// 取值域 {timeout, error} 由 schema 约束，此处不再枚举。
func failureReason(failure json.RawMessage) string {
	var f struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(failure, &f); err != nil {
		return ""
	}
	return f.Reason
}

// faultTimes faults 记录里的三个时间字段。
var faultTimes = []string{"injected_at", "recovered_at", "verified_at"}

// checkFaults 协议 v1.1 后置校验——schema 表达不了的两类规则：
//   - 时间戳须为 RFC3339 带时区：schema 的 format: date-time 在 v6 默认不校验（同 events 的 ts，day34 裁决）；
//   - 时间先后：recovered_at 不早于 injected_at，verified_at 不早于 recovered_at（各取非 null 者）。
//
// 返回记录条数（供调用方留痕）。未携带 faults 时返回 (0, nil)。
// 逐键取原始值再解：日后 schema 给记录加非字符串字段，也不会把合法请求误判成 400。
func checkFaults(faults json.RawMessage) (int, error) {
	if len(faults) == 0 {
		return 0, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(faults, &items); err != nil {
		return 0, err // schema 已保证可解，防御分支
	}
	for i, item := range items {
		parsed := map[string]time.Time{}
		for _, name := range faultTimes {
			raw, ok := item[name]
			if !ok || string(raw) == "null" {
				continue
			}
			var ts string
			if err := json.Unmarshal(raw, &ts); err != nil {
				return 0, fmt.Errorf("faults[%d].%s 须为字符串时间戳", i, name)
			}
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return 0, fmt.Errorf("faults[%d].%s 需为 RFC3339 带时区: %q", i, name, ts)
			}
			parsed[name] = t
		}
		if recovered, ok := parsed["recovered_at"]; ok && parsed["injected_at"].After(recovered) {
			return 0, fmt.Errorf("faults[%d].recovered_at 不得早于 injected_at", i)
		}
		if verified, ok := parsed["verified_at"]; ok && parsed["recovered_at"].After(verified) {
			return 0, fmt.Errorf("faults[%d].verified_at 不得早于 recovered_at", i)
		}
	}
	return len(items), nil
}

// checkJSONBStorable 回传体的 summary / reconciliation / failure 会原样进 jsonb 列。PostgreSQL 的 jsonb 拒绝
// \u0000 转义与非法 UTF-8 字节（22P05），而 schema 放行——不在入口拦就会变成落库时 500，Agent 重试多少次都过不去。
// failure.message 取自日志文本，是最可能带进这类内容的字段。
func checkJSONBStorable(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("请求体含非法 UTF-8 字节（PostgreSQL jsonb 无法存储）")
	}
	if bytes.Contains(raw, []byte(`\u0000`)) {
		return errors.New(`请求体含 NUL 字符（\u0000，PostgreSQL jsonb 无法存储）`)
	}
	return nil
}

// metricUnits 五指标单位映射（表 23：率类 %、时长 s）。
// 率类原始值域 [0,1]（metrics.py 契约）——入库经 toStoredValue 换算为百分数值，value 与 unit 才自洽
// （否则 1.0 配 "%" 会被读成 1%）。注意口径分叉：test_result.metrics 的 jsonb（含 details 的 value/actual）
// 保持原始 [0,1]，只有 test_metric_summary 是百分数——读数时看表，别跨表直接比。
var metricUnits = map[string]string{
	"processed_rate": "%", "dup_rate": "%", "loss_rate": "%",
	"avg_delivery_latency_s": "s", "recovery_time_s": "s",
}

// toStoredValue 指标值换算为存储口径：率类 ×100（保留 4 位小数，消除二进制浮点尾差），其余原值。
func toStoredValue(key string, v float64) float64 {
	if metricUnits[key] == "%" {
		return math.Round(v*1e6) / 1e4
	}
	return v
}

// summaryVerdict 从 summary 提取 verdict（test_result.verdict，表 22）。
func summaryVerdict(summary json.RawMessage) string {
	var s struct {
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal(summary, &s) // schema 已保证键存在
	return s.Verdict
}

// summaryMetrics 提取五指标的**存储口径**数值（率类已换算百分数）。宽松解码 map[string]any（summary 含
// verdict/details 等非数值键，严格 map[string]json.Number 会整包失败）——逐键 float64 断言；
// null/缺失自然跳过（null=场景不适用）。
func summaryMetrics(summary json.RawMessage) map[string]float64 {
	var m map[string]any
	if err := json.Unmarshal(summary, &m); err != nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(metricUnits))
	for key := range metricUnits {
		if f, ok := m[key].(float64); ok {
			out[key] = toStoredValue(key, f)
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
	faultCount, err := checkFaults(req.Faults)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	if err := checkJSONBStorable(raw); err != nil {
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
	if len(req.Failure) > 0 { // v1.1：失败原因原文（仅 FAILED 携带）；其余状态保持 NULL
		failure := string(req.Failure)
		r.Failure = &failure
	}
	if err := s.repo.CreateResult(r); err != nil {
		if errors.Is(err, ErrResultExists) {
			return nil // 并发迟到方：幂等语义 200
		}
		return fmt.Errorf("create result: %w", err)
	}
	if faultCount > 0 {
		// v1.1 只定了 wire 形状：故障记录的落库（fault_injection 表）随 day53 实现，在此之前校验后不持久化——
		// 留痕以免"带了却查不到"被当成静默丢失。放在落库之后：被拒与重复的回传不算"带了却查不到"。
		log.Printf("task: result 携带 %d 条故障记录（协议 v1.1，fault_injection 落库随 day53，当前仅校验）task=%d eid=%s", faultCount, exec.TaskID, eid)
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
	if err := s.repo.FinishExecution(eid, execTerminalSubstate(req.Status), exitReasonOf(req.Status, req.Failure)); err != nil {
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
