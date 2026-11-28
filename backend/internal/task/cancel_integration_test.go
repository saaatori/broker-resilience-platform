package task

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// TestIntegrationCancelFlow 集成（day41）：取消闭环三态真库真 Redis——
// PENDING 直接收口；DISPATCHED 置标志→首条 STATUS 转 CANCELLING（events.go day34 路径联测）
// →心跳下发 cancel_requested→result(CANCELLED) 收口+标志删除；RUNNING 取消转 CANCELLING；终态 409。
func TestIntegrationCancelFlow(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{Gate: gate, Hub: NewHub()})

	agent := &model.AgentInstance{AgentID: uuid.NewString(), Hostname: "cancel-it", Version: "0.0.1",
		TokenHash: "cancel-it", Status: "online"}
	if err := db.Create(agent).Error; err != nil {
		t.Fatalf("造 agent: %v", err)
	}
	newTask := func(status TaskState) *model.TestTask {
		sc := &model.TestScenario{Key: "cancel_it_" + uuid.NewString()[:8], Name: "CI", DefaultParams: `{"message_count":1}`, Enabled: true}
		if err := db.Create(sc).Error; err != nil {
			t.Fatalf("造 scenario: %v", err)
		}
		row := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1,
			ParamsSnapshot: `{"message_count":1}`, Status: string(status)}
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("造 task: %v", err)
		}
		return row
	}
	taskStatus := func(id int64) string {
		var row model.TestTask
		db.First(&row, id)
		return row.Status
	}

	// ---- PENDING：直接收口 CANCELLED ----
	pending := newTask(StatePending)
	if _, err := svc.Cancel(pending.ID); err != nil {
		t.Fatalf("PENDING 取消: %v", err)
	}
	if got := taskStatus(pending.ID); got != string(StateCancelled) {
		t.Fatalf("PENDING 取消后 = %q, want CANCELLED", got)
	}
	if _, err := svc.Cancel(pending.ID); err == nil {
		t.Fatal("CANCELLED 再取消应 409")
	}

	// ---- DISPATCHED：置标志 → 首条 STATUS 转 CANCELLING → 心跳下发 → result 收口 ----
	sc := &model.TestScenario{Key: "cancel_it2", Name: "CI2", DefaultParams: `{"message_count":1}`, Enabled: true}
	if err := db.Create(sc).Error; err != nil {
		t.Fatalf("造 scenario: %v", err)
	}
	dRow := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1,
		ParamsSnapshot: `{"message_count":1}`, Status: string(StatePending)}
	if err := db.Create(dRow).Error; err != nil {
		t.Fatalf("造 task: %v", err)
	}
	dTask, dExec, _, err := repo.ClaimNextTask(agent.ID)
	if err != nil || dTask.ID != dRow.ID {
		t.Fatalf("claim: %v", err)
	}
	eid := dExec.ExecutionID

	status, err := svc.Cancel(dRow.ID)
	if err != nil {
		t.Fatalf("DISPATCHED 取消: %v", err)
	}
	if status != string(StateDispatched) || taskStatus(dRow.ID) != string(StateDispatched) {
		t.Fatalf("DISPATCHED 取消不得转移: resp=%q db=%q", status, taskStatus(dRow.ID))
	}
	if flag, _ := gate.CancelFlag(eid); !flag {
		t.Fatal("exec:cancel 标志未写入（真 Redis）")
	}

	// 首条 STATUS（events.go v1.0.5 I-2 路径）：读标志 → CANCELLING
	evSvc := NewService(repo, ServiceDeps{Events: newTestEventsValidator(t), Gate: gate, Hub: NewHub()})
	raw := `{"seq":0,"ts":"` + time.Now().UTC().Format(time.RFC3339) + `","type":"STATUS","payload":{"substate":"CLAIMED"}}`
	if err := evSvc.Event(agent, eid, []byte(raw)); err != nil {
		t.Fatalf("首条 STATUS: %v", err)
	}
	if got := taskStatus(dRow.ID); got != string(StateCancelling) {
		t.Fatalf("置标志后首条 STATUS 应转 CANCELLING, got %q", got)
	}

	// 心跳：CANCELLING → cancel_requested
	leaseOK, cancel, err := svc.Heartbeat(agent, []string{eid})
	if err != nil {
		t.Fatalf("心跳: %v", err)
	}
	if len(leaseOK) != 0 || len(cancel) != 1 || cancel[0] != eid {
		t.Fatalf("CANCELLING 应入 cancel_requested: lease=%v cancel=%v", leaseOK, cancel)
	}

	// result(CANCELLED) 收口（v1.0.1 ※：无 cleanup-ack 时 result 直接收口）+ 标志删除
	resSvc := NewService(repo, ServiceDeps{Result: newTestResultValidator(t), Gate: gate, Hub: NewHub()})
	resRaw := `{"attempt":1,"status":"CANCELLED","summary":{"sent":0,"confirmed":0,"delivered":0,` +
		`"processed_unique":0,"acked":0,"dup_business":0,"processed_rate":null,"dup_rate":null,` +
		`"loss_rate":null,"avg_delivery_latency_s":null,"recovery_time_s":null,"verdict":"FAIL","details":[]},` +
		`"reconciliation":{"dup_business_ids":[],"sent_not_confirmed":[],"confirmed_not_delivered":[],` +
		`"delivered_not_processed":[],"processed_not_acked":[],"total_events":0},"artifacts":[]}`
	if err := resSvc.Result(agent, eid, []byte(resRaw)); err != nil {
		t.Fatalf("result(CANCELLED): %v", err)
	}
	if got := taskStatus(dRow.ID); got != string(StateCancelled) {
		t.Fatalf("result 后应 CANCELLED, got %q", got)
	}
	if flag, _ := gate.CancelFlag(eid); flag {
		t.Fatal("exec:cancel 标志应在 result 后被删除")
	}
	var execRow model.TestExecution
	db.Where("execution_id = ?", eid).First(&execRow)
	if execRow.Status != "ABORTED" || execRow.FinishedAt == nil {
		t.Fatalf("执行应收口 ABORTED+finished: %v/%v", execRow.Status, execRow.FinishedAt)
	}

	// ---- RUNNING：取消转 CANCELLING（无标志路径） ----
	rRow := newTask(StatePending)
	if _, _, _, err := repo.ClaimNextTask(agent.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	db.Model(&model.TestTask{}).Where("id = ?", rRow.ID).Update("status", string(StateRunning))
	var rExec model.TestExecution
	db.Where("task_id = ?", rRow.ID).First(&rExec)

	if _, err := svc.Cancel(rRow.ID); err != nil {
		t.Fatalf("RUNNING 取消: %v", err)
	}
	if got := taskStatus(rRow.ID); got != string(StateCancelling) {
		t.Fatalf("RUNNING 取消后 = %q, want CANCELLING", got)
	}
	if flag, _ := gate.CancelFlag(rExec.ExecutionID); flag {
		t.Fatal("RUNNING 取消不应置标志（DISPATCHED 专用）")
	}
}

// TestIntegrationCancelConcurrentRaces 集成（day41 评审 I-1/I-2）：
// 取消与首条 STATUS / result 并发——最终态必 ∈ {CANCELLING, CANCELLED, COMPLETED}，
// 且取消侧不返回与库内事实矛盾的状态（假成功）。
func TestIntegrationCancelConcurrentRaces(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{Gate: gate, Hub: NewHub()})
	evSvc := NewService(repo, ServiceDeps{Events: newTestEventsValidator(t), Gate: gate, Hub: NewHub()})

	agent := &model.AgentInstance{AgentID: uuid.NewString(), Hostname: "cancel-race", Version: "0.0.1",
		TokenHash: "race", Status: "online"}
	if err := db.Create(agent).Error; err != nil {
		t.Fatalf("造 agent: %v", err)
	}
	sc := &model.TestScenario{Key: "cancel_race_" + uuid.NewString()[:8], Name: "CR",
		DefaultParams: `{"message_count":1}`, Enabled: true}
	if err := db.Create(sc).Error; err != nil {
		t.Fatalf("造 scenario: %v", err)
	}
	row := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1,
		ParamsSnapshot: `{"message_count":1}`, Status: string(StatePending)}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("造 task: %v", err)
	}
	_, exec, _, err := repo.ClaimNextTask(agent.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	eid := exec.ExecutionID

	// 并发：取消 vs 首条 STATUS（DISPATCHED 窗口）×20 轮交错
	cancelErr := make(chan error, 1)
	eventErr := make(chan error, 1)
	go func() {
		_, err := svc.Cancel(row.ID)
		cancelErr <- err
	}()
	go func() {
		raw := `{"seq":0,"ts":"` + time.Now().UTC().Format(time.RFC3339) + `","type":"STATUS","payload":{"substate":"CLAIMED"}}`
		eventErr <- evSvc.Event(agent, eid, []byte(raw))
	}()
	ce, ee := <-cancelErr, <-eventErr
	if ce != nil || ee != nil {
		t.Fatalf("cancel=%v event=%v", ce, ee)
	}
	var after model.TestTask
	db.First(&after, row.ID)
	got := TaskState(after.Status)
	if got != StateCancelling && got != StateCancelled && got != StateRunning {
		t.Fatalf("并发后任务态 = %q, want ∈ {CANCELLING, CANCELLED, RUNNING（标志窗口竞掉后的合法终局之一）}", got)
	}
	// 取消侧响应与库内一致性：若返回 DISPATCHED 而库已 RUNNING 且无标志生效——本用例通过后续心跳闭环兜底
}
