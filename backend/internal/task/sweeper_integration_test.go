package task

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// openTestCleanupQueue 集成队列（与 openTestEventGate 同一 DB1 键空间）。
func openTestCleanupQueue(t *testing.T) *RedisCleanupQueue {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", Password: "brp_local_dev", DB: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("测试 Redis 不可达: %v", err)
	}
	rdb.Del(ctx, cleanupQueueKey) // 清队列避免跨轮污染
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisCleanupQueue(rdb)
}

// seedTwoTasks 造一场景两任务（key 参数化避免 uq 冲突）。
func seedTwoTasks(t *testing.T, db *gorm.DB, key string) (taskA, taskB *model.TestTask) {
	t.Helper()
	sc := &model.TestScenario{Key: key, Name: "IT", DefaultParams: `{"message_count":1}`, Enabled: true}
	if err := db.Create(sc).Error; err != nil {
		t.Fatalf("造 scenario: %v", err)
	}
	for i := 0; i < 2; i++ {
		row := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1,
			ParamsSnapshot: `{"message_count":1}`, Status: string(StatePending)}
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("造 task: %v", err)
		}
		if i == 0 {
			taskA = row
		} else {
			taskB = row
		}
	}
	return
}

// TestIntegrationSweepExpired 集成：claim×2→A 置过期→SweepOnce→全链断言；
// B 未过期对照不扫；清扫后心跳自洽（day33 M-2 闭环：ABORTED 不入 lease_ok）。
func TestIntegrationSweepExpired(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	queue := openTestCleanupQueue(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{Events: newTestEventsValidator(t), Gate: gate,
		Hub: NewHub(), CleanupQueue: queue})

	reg, err := svc.Register("sweep-it-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agentRow, _ := repo.FindAgentByUUID(reg.AgentUUID)
	taskA, taskB := seedTwoTasks(t, db, "sweep_it_scenario")
	_, execA, _, err := repo.ClaimNextTask(agentRow.ID)
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	_, execB, _, err := repo.ClaimNextTask(agentRow.ID)
	if err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if err := db.Model(&model.TestExecution{}).Where("execution_id = ?", execA.ExecutionID).
		Update("lease_until", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("A 置过期: %v", err)
	}

	n, err := svc.SweepOnce(time.Now())
	if err != nil || n != 1 {
		t.Fatalf("应恰扫 1 条（B 未过期）: n=%d err=%v", n, err)
	}
	var taskADB model.TestTask
	db.First(&taskADB, taskA.ID)
	if taskADB.Status != string(StateFailed) {
		t.Fatalf("A 应 FAILED: %s", taskADB.Status)
	}
	var execADB model.TestExecution
	db.Where("execution_id = ?", execA.ExecutionID).First(&execADB)
	if execADB.Status != string(execStatusAborted) || execADB.ExitReason == nil || *execADB.ExitReason != leaseLostReason || execADB.FinishedAt == nil {
		t.Fatalf("A exec 应 ABORTED/lease_lost/finished_at: %+v", execADB)
	}
	var taskBDB model.TestTask
	db.First(&taskBDB, taskB.ID)
	if taskBDB.Status != string(StateDispatched) {
		t.Fatalf("B 未过期不得扫: %s", taskBDB.Status)
	}
	// 队列恰含 A
	if q, err := queue.client.LLen(context.Background(), cleanupQueueKey).Result(); err != nil || q != 1 {
		t.Fatalf("cleanup:queue 应 1 条: q=%d err=%v", q, err)
	}
	// 心跳自洽：A（ABORTED）不入 lease_ok；B 正常
	leaseOK, cancel, err := svc.Heartbeat(agentRow, []string{execA.ExecutionID, execB.ExecutionID})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(leaseOK) != 1 || leaseOK[0] != execB.ExecutionID || len(cancel) != 0 {
		t.Fatalf("lease_ok 应恰 B、cancel 空: %v %v", leaseOK, cancel)
	}
}

// TestIntegrationSweepScanSet 扫描集边界（day37 评审 I-2）：终态+过期不入集；中断窗口（exec DONE+任务
// RUNNING+过期）入集；I-1 钉子：FinishExecution 对已终态 exec 不覆盖（RowsAffected=0 幂等放弃）。
func TestIntegrationSweepScanSet(t *testing.T) {
	db := openIntegrationDB(t)
	repo := NewRepository(db)
	_, taskFin := seedTwoTasks(t, db, "sweep_scan_fin")
	_, taskInt := seedTwoTasks(t, db, "sweep_scan_int")
	for _, row := range []*model.TestTask{taskFin, taskInt} {
		db.Model(&model.TestTask{}).Where("id = ?", row.ID).Update("status", string(StateRunning))
	}
	// 终态任务 + 过期执行
	db.Model(&model.TestTask{}).Where("id = ?", taskFin.ID).Update("status", string(StateCompleted))
	finExec := &model.TestExecution{ExecutionID: "22222222-2222-4222-8222-222222222221", TaskID: taskFin.ID,
		Attempt: 1, AgentID: 1, Status: "CLAIMED", LeaseUntil: ptrTimePass()}
	db.Create(finExec)
	// 中断窗口：任务 RUNNING + exec DONE（result 已收口——真实收口必带 finished_at）+ 过期
	finished := ptrTimePass()
	intExec := &model.TestExecution{ExecutionID: "22222222-2222-4222-8222-222222222222", TaskID: taskInt.ID,
		Attempt: 1, AgentID: 1, Status: string(execStatusDone), LeaseUntil: ptrTimePass(), FinishedAt: finished}
	db.Create(intExec)

	views, err := repo.FindLeaseExpired(time.Now())
	if err != nil {
		t.Fatalf("扫描: %v", err)
	}
	var hitInt, hitFin bool
	for _, v := range views {
		if v.Exec.ExecutionID == intExec.ExecutionID {
			hitInt = true
		}
		if v.Exec.ExecutionID == finExec.ExecutionID {
			hitFin = true
		}
	}
	if !hitInt {
		t.Fatal("中断窗口（exec DONE+任务 RUNNING+过期）应入扫描集")
	}
	if hitFin {
		t.Fatal("任务终态不得入扫描集")
	}

	// I-1 钉子：对已被 result 收口的 exec（DONE + finished_at）调 FinishExecution(ABORTED) 不覆盖。
	// 造数须带 finished_at——守卫按"是否已收口"判定；只给 DONE 不给 finished_at 的是"终态子状态先到、
	// result 未到"，那种执行正应当被随后的收口写入（见 TestIntegrationResultAfterTerminalSubstate）。
	if err := repo.FinishExecution(intExec.ExecutionID, string(execStatusAborted), leaseLostReason); err != nil {
		t.Fatalf("FinishExecution: %v", err)
	}
	var after model.TestExecution
	db.Where("execution_id = ?", intExec.ExecutionID).First(&after)
	if after.Status != string(execStatusDone) {
		t.Fatalf("exec DONE 不得被覆盖 ABORTED: %s", after.Status)
	}
}

func ptrTimePass() *time.Time {
	t := time.Now().Add(-time.Minute)
	return &t
}
