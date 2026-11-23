package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// openTestEventGate 真 Redis 闸门（compose 的 localhost:6379）；不可达即跳过。
// 幂等行为（SETNX/TTL）只有真 Redis 可验——fake 只证语义接线。
func openTestEventGate(t *testing.T) *RedisEventGate {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", Password: "brp_local_dev", DB: 1}) // DB1：与生产键空间隔离
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("测试 Redis 不可达（compose 栈需在跑）: %v", err)
	}
	// DB1 为测试专用键空间；清 exec:evt:* 避免跨轮污染（不影响 DB0 生产键）
	keys, _ := rdb.Keys(ctx, "exec:evt:*").Result()
	if len(keys) > 0 {
		rdb.Del(ctx, keys...)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisEventGate(rdb)
}

func TestIntegrationEventFlow(t *testing.T) {
	db := openIntegrationDB(t)
	gate := openTestEventGate(t)
	repo := NewRepository(db)
	svc := NewService(repo, ServiceDeps{Events: newTestEventsValidator(t), Gate: gate, Hub: NewHub()})

	// 造数：agent + claim
	reg, err := svc.Register("evt-it-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agentRow, err := repo.FindAgentByUUID(reg.AgentUUID)
	if err != nil {
		t.Fatalf("find agent: %v", err)
	}
	_, taskRow := seedScenarioAndTask(t, db)
	_, exec, _, err := repo.ClaimNextTask(agentRow.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	statusJSON := `{"seq":0,"ts":"2026-11-21T10:00:00+00:00","type":"STATUS","payload":{"substate":"PREPARING"}}`
	if err := svc.Event(agentRow, exec.ExecutionID, []byte(statusJSON)); err != nil {
		t.Fatalf("首条 STATUS: %v", err)
	}
	var taskDB model.TestTask
	db.First(&taskDB, taskRow.ID)
	if taskDB.Status != "RUNNING" {
		t.Fatalf("task 应 RUNNING: %s", taskDB.Status)
	}
	var execDB model.TestExecution
	db.Where("execution_id = ?", exec.ExecutionID).First(&execDB)
	if execDB.Status != "PREPARING" {
		t.Fatalf("exec 子状态应 PREPARING: %s", execDB.Status)
	}

	// 重放同 seq：nil 且 DB 无变化（幂等去重——验收原文"重放同 seq 事件不重复"的 DB 侧）
	if err := svc.Event(agentRow, exec.ExecutionID, []byte(statusJSON)); err != nil {
		t.Fatalf("重放应 nil: %v", err)
	}
	db.First(&taskDB, taskRow.ID)
	db.Where("execution_id = ?", exec.ExecutionID).First(&execDB)
	if taskDB.Status != "RUNNING" || execDB.Status != "PREPARING" {
		t.Fatal("重放不应产生任何 DB 效应")
	}

	// 他人 eid → 404 哨兵
	other := &model.AgentInstance{ID: agentRow.ID + 999, AgentID: reg.AgentUUID}
	if err := svc.Event(other, exec.ExecutionID, []byte(statusJSON)); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("他人 eid 应 404 哨兵: %v", err)
	}
	if err := svc.Event(agentRow, "00000000-0000-0000-0000-00000000beef", []byte(statusJSON)); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("不存在 eid 应 404 哨兵: %v", err)
	}
}

// seedScenarioAndTask 集成造数（复用 seedClaimFixture 的场景+任务部分，agent 自建）。
func seedScenarioAndTask(t *testing.T, db *gorm.DB) (*model.TestScenario, *model.TestTask) {
	t.Helper()
	sc := &model.TestScenario{Key: "evt_it_scenario", Name: "IT", DefaultParams: `{"message_count":1}`, Enabled: true}
	if err := db.Create(sc).Error; err != nil {
		t.Fatalf("造 scenario: %v", err)
	}
	taskRow := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1, ParamsSnapshot: `{"message_count":1}`, Status: string(StatePending)}
	if err := db.Create(taskRow).Error; err != nil {
		t.Fatalf("造 task: %v", err)
	}
	return sc, taskRow
}
