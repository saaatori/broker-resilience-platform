package task

import (
	"errors"
	"sync"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// TestIntegrationResultFlow 集成（真 PG+Redis）：claim→STATUS→result(COMPLETED) 三表落库；
// first-wins 重放零新增；终态后心跳不再刷新（day33 M-2 验收断言）。
func TestIntegrationResultFlow(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{
		Events: newTestEventsValidator(t), Result: newTestResultValidator(t), Gate: gate, Hub: NewHub(),
	})

	reg, err := svc.Register("res-it-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agentRow, _ := repo.FindAgentByUUID(reg.AgentUUID)
	_, taskRow := seedScenarioAndTask(t, db)
	_, exec, _, err := repo.ClaimNextTask(agentRow.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// 首条 STATUS → RUNNING
	statusJSON := []byte(`{"seq":0,"ts":"2026-11-22T10:00:00+00:00","type":"STATUS","payload":{"substate":"VERIFYING"}}`)
	if err := svc.Event(agentRow, exec.ExecutionID, statusJSON); err != nil {
		t.Fatalf("STATUS: %v", err)
	}

	// result(COMPLETED)
	if err := svc.Result(agentRow, exec.ExecutionID, resultJSON("COMPLETED", 1)); err != nil {
		t.Fatalf("result: %v", err)
	}
	var taskDB model.TestTask
	db.First(&taskDB, taskRow.ID)
	if taskDB.Status != "COMPLETED" {
		t.Fatalf("task 应 COMPLETED: %s", taskDB.Status)
	}
	var execDB model.TestExecution
	db.Where("execution_id = ?", exec.ExecutionID).First(&execDB)
	if execDB.Status != "DONE" {
		t.Fatalf("exec 应 DONE: %s", execDB.Status)
	}
	var resultCount, summaryCount, reportCount int64
	db.Model(&model.TestResult{}).Where("execution_id = ?", exec.ExecutionID).Count(&resultCount)
	db.Model(&model.TestMetricSummary{}).Where("execution_id = ?", exec.ExecutionID).Count(&summaryCount)
	db.Model(&model.TestReport{}).Where("execution_id = ?", exec.ExecutionID).Count(&reportCount)
	if resultCount != 1 || summaryCount != 4 || reportCount != 2 { // recovery_time_s=null 跳过
		t.Fatalf("三表行数不符: result=%d summary=%d report=%d", resultCount, summaryCount, reportCount)
	}
	var verdict model.TestResult
	db.Where("execution_id = ?", exec.ExecutionID).First(&verdict)
	if verdict.Verdict != "PASS" {
		t.Fatalf("verdict 应 PASS: %s", verdict.Verdict)
	}

	// first-wins 重放（同 result + 尾随 CANCELLED）：零新增、状态不变
	if err := svc.Result(agentRow, exec.ExecutionID, resultJSON("COMPLETED", 1)); err != nil {
		t.Fatalf("重放应 nil: %v", err)
	}
	if err := svc.Result(agentRow, exec.ExecutionID, resultJSON("CANCELLED", 1)); err != nil {
		t.Fatalf("尾随应 nil: %v", err)
	}
	db.Model(&model.TestResult{}).Where("execution_id = ?", exec.ExecutionID).Count(&resultCount)
	db.Model(&model.TestReport{}).Where("execution_id = ?", exec.ExecutionID).Count(&reportCount)
	db.First(&taskDB, taskRow.ID)
	if resultCount != 1 || reportCount != 2 || taskDB.Status != "COMPLETED" {
		t.Fatalf("重放/尾随产生副作用: result=%d report=%d status=%s", resultCount, reportCount, taskDB.Status)
	}

	// day33 M-2 验收断言：终态执行（DONE）心跳不再刷新（judgeExec 排除 → 差集）
	leaseOK, cancel, err := svc.Heartbeat(agentRow, []string{exec.ExecutionID})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(leaseOK) != 0 || len(cancel) != 0 {
		t.Fatalf("终态执行不应入任一数组: lease=%v cancel=%v", leaseOK, cancel)
	}
}

// TestIntegrationResultConcurrentDuplicate 并发双写 uq：一成一生成 ErrResultExists
// ——TranslateError 生效的集成钉子（day35 评审 I-1：此前 23505 不译码，迟到方 500）。
func TestIntegrationResultConcurrentDuplicate(t *testing.T) {
	db := openIntegrationDB(t)
	repo := NewRepository(db)
	const eid = "11111111-1111-1111-1111-111111111111"

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.CreateResult(&model.TestResult{ExecutionID: eid, Verdict: "PASS", Metrics: "{}", Reconciliation: "{}"})
		}()
	}
	wg.Wait()
	close(errs)

	okCount, dupCount := 0, 0
	for e := range errs {
		switch {
		case e == nil:
			okCount++
		case errors.Is(e, ErrResultExists):
			dupCount++
		default:
			t.Fatalf("并发双写出现意外错误: %v", e)
		}
	}
	if okCount != 1 || dupCount != 1 {
		t.Fatalf("应恰 1 成功 + 1 ErrResultExists（TranslateError 映射生效），实际 ok=%d dup=%d", okCount, dupCount)
	}
}
