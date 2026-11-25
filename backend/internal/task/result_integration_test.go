package task

import (
	"encoding/json"
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

// TestIntegrationResultFailurePersisted 集成（真 PG，协议 v1.1）：失败原因端到端落库——
// exit_reason 取 failure.reason（timeout），failure 原文进 test_result.failure（jsonb，AutoMigrate 新增列）。
// 该用例在空库上跑即同时证明：生产迁移清单能把新列带到既有库结构之外的环境。
func TestIntegrationResultFailurePersisted(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{
		Events: newTestEventsValidator(t), Result: newTestResultValidator(t), Gate: gate, Hub: NewHub(),
	})

	reg, err := svc.Register("res-fail-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agentRow, _ := repo.FindAgentByUUID(reg.AgentUUID)
	_, taskRow := seedScenarioAndTask(t, db)
	_, exec, _, err := repo.ClaimNextTask(agentRow.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	raw := resultJSONExtra("FAILED", 1, `"failure":{"reason":"timeout","message":"total timeout 300s"}`)
	if err := svc.Result(agentRow, exec.ExecutionID, raw); err != nil {
		t.Fatalf("result: %v", err)
	}
	var taskDB model.TestTask
	db.First(&taskDB, taskRow.ID)
	if taskDB.Status != "FAILED" {
		t.Fatalf("task 应 FAILED: %s", taskDB.Status)
	}
	var execDB model.TestExecution
	db.Where("execution_id = ?", exec.ExecutionID).First(&execDB)
	if execDB.Status != "DONE" || execDB.ExitReason == nil || *execDB.ExitReason != "timeout" {
		t.Fatalf("exec 应 DONE/timeout: status=%s exit_reason=%v", execDB.Status, execDB.ExitReason)
	}
	var resDB model.TestResult
	db.Where("execution_id = ?", exec.ExecutionID).First(&resDB)
	// jsonb 列读回的是 PG 规范化文本（冒号后带空格、键有序）——按内容断言，不比字面。
	var stored struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if resDB.Failure == nil || json.Unmarshal([]byte(*resDB.Failure), &stored) != nil || stored.Reason != "timeout" {
		t.Fatalf("failure 应落 test_result.failure 且 reason=timeout: %v", resDB.Failure)
	}
	var colType string
	db.Raw(`SELECT data_type FROM information_schema.columns
	        WHERE table_name = 'test_result' AND column_name = 'failure'`).Scan(&colType)
	if colType != "jsonb" {
		t.Fatalf("test_result.failure 列类型应 jsonb: %q", colType)
	}
}

// TestIntegrationResultAfterTerminalSubstate 集成（真 PG，2026-11-24 评审 I-1）：
// 协议自己的时序里，Agent 会先经 STATUS 上报终态子状态、再回传 result——取消闭环是 ABORTED → result(CANCELLED)
// （§4），超时强杀也可能先报终态子状态。STATUS 会把 DONE / ABORTED 写进执行状态；FinishExecution 的守卫
// 若按"状态非终态"判定，随后的 result 就是 0 行更新，exit_reason 与 finished_at 全部丢失。
// 守卫应按"是否已被收口"（finished_at）判定：终态子状态先到的执行，仍须由 result 收口。
func TestIntegrationResultAfterTerminalSubstate(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{
		Events: newTestEventsValidator(t), Result: newTestResultValidator(t), Gate: gate, Hub: NewHub(),
	})

	reg, err := svc.Register("res-term-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agentRow, _ := repo.FindAgentByUUID(reg.AgentUUID)
	seedTwoTasks(t, db, "res_term_scenario")
	claim := func() *model.TestExecution {
		_, exec, _, err := repo.ClaimNextTask(agentRow.ID)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return exec
	}
	reportSubstate := func(eid, sub string) {
		raw := []byte(`{"seq":0,"ts":"2026-11-24T10:00:00+00:00","type":"STATUS","payload":{"substate":"` + sub + `"}}`)
		if err := svc.Event(agentRow, eid, raw); err != nil {
			t.Fatalf("STATUS(%s): %v", sub, err)
		}
	}

	// 取消闭环时序（协议 §4）：子状态 ABORTED 先到，result(CANCELLED) 后到
	execA := claim()
	reportSubstate(execA.ExecutionID, "ABORTED")
	if err := svc.Result(agentRow, execA.ExecutionID, resultJSON("CANCELLED", 1)); err != nil {
		t.Fatalf("result(CANCELLED): %v", err)
	}
	// 终态子状态先到的 FAILED：exit_reason 须取自 failure.reason（v1.1）
	execB := claim()
	reportSubstate(execB.ExecutionID, "DONE")
	raw := resultJSONExtra("FAILED", 1, `"failure":{"reason":"timeout","message":"total timeout 300s"}`)
	if err := svc.Result(agentRow, execB.ExecutionID, raw); err != nil {
		t.Fatalf("result(FAILED): %v", err)
	}

	cases := []struct {
		eid, wantStatus, wantReason string
	}{
		{execA.ExecutionID, "ABORTED", "cancelled"},
		{execB.ExecutionID, "DONE", "timeout"},
	}
	for _, c := range cases {
		var execDB model.TestExecution
		db.Where("execution_id = ?", c.eid).First(&execDB)
		if execDB.Status != c.wantStatus || execDB.ExitReason == nil || *execDB.ExitReason != c.wantReason || execDB.FinishedAt == nil {
			t.Fatalf("终态子状态先到时 result 仍须收口: want %s/%s/finished_at, got %s/%v/%v",
				c.wantStatus, c.wantReason, execDB.Status, execDB.ExitReason, execDB.FinishedAt)
		}
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
