package task

import (
	"testing"
	"time"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// TestIntegrationHeartbeatRenewsLease 集成：claim → 两次心跳，lease_until 连续递增；
// CANCELLING 任务（手工置态）→ cancel_requested 且 lease_until 不被刷新（day33 裁决）。
func TestIntegrationHeartbeatRenewsLease(t *testing.T) {
	db := openIntegrationDB(t)
	_, taskRow, agent := seedClaimFixture(t, db)
	repo := NewRepository(db)
	svc := NewService(repo, nil, nil, nil, nil)

	_, exec, _, err := repo.ClaimNextTask(agent.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	eid := exec.ExecutionID

	leaseOK, cancel, err := svc.Heartbeat(agent, []string{eid})
	if err != nil {
		t.Fatalf("心跳1: %v", err)
	}
	if len(leaseOK) != 1 || leaseOK[0] != eid || len(cancel) != 0 {
		t.Fatalf("心跳1 双数组不符: lease=%v cancel=%v", leaseOK, cancel)
	}
	var exec1 model.TestExecution
	db.Where("execution_id = ?", eid).First(&exec1)

	time.Sleep(1100 * time.Millisecond) // 保证 lease_until 严格递增可观测
	leaseOK, _, err = svc.Heartbeat(agent, []string{eid})
	if err != nil {
		t.Fatalf("心跳2: %v", err)
	}
	if len(leaseOK) != 1 {
		t.Fatalf("心跳2 应仍认可: %v", leaseOK)
	}
	var exec2 model.TestExecution
	db.Where("execution_id = ?", eid).First(&exec2)
	if !exec2.LeaseUntil.After(*exec1.LeaseUntil) {
		t.Fatalf("lease_until 应连续刷新递增: %v → %v", exec1.LeaseUntil, exec2.LeaseUntil)
	}

	// CANCELLING：入 cancel_requested 且不再刷新（仅 lease_ok 刷新）
	db.Model(&model.TestTask{}).Where("id = ?", taskRow.ID).Update("status", taskStatusCancelling)
	time.Sleep(1100 * time.Millisecond)
	leaseOK, cancel, err = svc.Heartbeat(agent, []string{eid})
	if err != nil {
		t.Fatalf("心跳3: %v", err)
	}
	if len(leaseOK) != 0 || len(cancel) != 1 || cancel[0] != eid {
		t.Fatalf("CANCELLING 应入 cancel_requested: lease=%v cancel=%v", leaseOK, cancel)
	}
	var exec3 model.TestExecution
	db.Where("execution_id = ?", eid).First(&exec3)
	if exec3.LeaseUntil.After(*exec2.LeaseUntil) {
		t.Fatal("cancel_requested 的执行不应被刷新租约（取消闭环限时 45s）")
	}

	// agent 存活信号落库
	var agentDB model.AgentInstance
	db.First(&agentDB, agent.ID)
	if agentDB.LastHeartbeatAt == nil || agentDB.Status != agentStatusOnline {
		t.Errorf("last_heartbeat_at/status 应更新: %+v", agentDB)
	}
}
