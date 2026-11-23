package task

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// openIntegrationDB 集成测试库（实施规划 §11"少量集成测试"）：
// BRP_TEST_DSN 未设即跳过（CI 无 PG 不红）；每测试 TRUNCATE 自清。
// 注意：TRUNCATE 会清空 test_task/test_scenario 等开发数据——验收证据以文件留档为准。
func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("BRP_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 BRP_TEST_DSN 指向测试 PG（例：host=localhost port=5432 user=brp password=brp_local_dev dbname=broker_resilience sslmode=disable）")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:         gormlogger.Discard,
		TranslateError: true, // 与生产 OpenPG 同口径（uq 冲突译码——day35 评审 I-1：测试连接漏配曾致穿透假象）
	})
	if err != nil {
		t.Fatalf("连测试库: %v", err)
	}
	if err := db.AutoMigrate(&model.TestScenario{}, &model.TestTask{}, &model.TestExecution{}, &model.AgentInstance{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := db.Exec("TRUNCATE test_task, test_execution, test_scenario, agent_instance, test_result, test_metric_summary, test_report RESTART IDENTITY").Error; err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	return db
}

// seedClaimFixture 造最小领取环境：场景 + 1 个 PENDING 任务 + 1 个 Agent，返回三者。
func seedClaimFixture(t *testing.T, db *gorm.DB) (*model.TestScenario, *model.TestTask, *model.AgentInstance) {
	t.Helper()
	agent := &model.AgentInstance{AgentID: uuid.NewString(), Hostname: "it-agent", Version: "0.0.1", TokenHash: "it", Status: "online"}
	if err := db.Create(agent).Error; err != nil {
		t.Fatalf("造 agent: %v", err)
	}
	sc := &model.TestScenario{Key: "it_scenario", Name: "IT", DefaultParams: `{"message_count":1}`, Enabled: true}
	if err := db.Create(sc).Error; err != nil {
		t.Fatalf("造 scenario: %v", err)
	}
	taskRow := &model.TestTask{ScenarioID: sc.ID, InstanceID: 1, CreatedBy: 1, ParamsSnapshot: `{"message_count":1}`, Status: string(StatePending)}
	if err := db.Create(taskRow).Error; err != nil {
		t.Fatalf("造 task: %v", err)
	}
	return sc, taskRow, agent
}

func TestIntegrationClaimSingle(t *testing.T) {
	db := openIntegrationDB(t)
	sc, taskRow, agent := seedClaimFixture(t, db)
	repo := NewRepository(db)

	gotTask, gotExec, key, err := repo.ClaimNextTask(agent.ID)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if gotTask.ID != taskRow.ID || gotTask.Status != "DISPATCHED" {
		t.Errorf("任务应转 DISPATCHED，实际 id=%d status=%s", gotTask.ID, gotTask.Status)
	}
	if gotExec.TaskID != taskRow.ID || gotExec.Attempt != 1 || gotExec.Status != "CLAIMED" {
		t.Errorf("execution 不符: %+v", gotExec)
	}
	if gotExec.AgentID != agent.ID {
		t.Errorf("execution.agent_id 应为领取者内部 id %d，实际 %d", agent.ID, gotExec.AgentID)
	}
	if gotExec.ExecutionID == "" {
		t.Error("execution_id 应为生成 uuid")
	}
	if _, err := uuid.Parse(gotExec.ExecutionID); err != nil {
		t.Errorf("execution_id 非合法 uuid: %v", err)
	}
	if key != sc.Key {
		t.Errorf("scenario_key 应为 %q，实际 %q", sc.Key, key)
	}
	if gotExec.StartedAt == nil {
		t.Error("started_at 应写入")
	}
	// 租约窗口（Review Focus 4）：45s 口径 ± 容差
	lease := time.Until(*gotExec.LeaseUntil)
	if lease < 43*time.Second || lease > 46*time.Second {
		t.Errorf("租约窗口应约 45s，实际 %v", lease)
	}
	// DB 复查：状态转移与 execution 行真实落库
	var taskDB model.TestTask
	db.First(&taskDB, taskRow.ID)
	if taskDB.Status != "DISPATCHED" {
		t.Errorf("DB 中任务状态应 DISPATCHED，实际 %s", taskDB.Status)
	}
	var execCount int64
	db.Model(&model.TestExecution{}).Where("task_id = ?", taskRow.ID).Count(&execCount)
	if execCount != 1 {
		t.Errorf("execution 应恰 1 行，实际 %d", execCount)
	}
}

func TestIntegrationClaimNoTask(t *testing.T) {
	db := openIntegrationDB(t)
	_, _, agent := seedClaimFixture(t, db)
	db.Model(&model.TestTask{}).Where("status = ?", string(StatePending)).Update("status", "DISPATCHED") // 清掉 PENDING
	repo := NewRepository(db)

	if _, _, _, err := repo.ClaimNextTask(agent.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("无 PENDING 应返回 ErrNoTask，实际 %v", err)
	}
}

func TestIntegrationFindAgentByUUID(t *testing.T) {
	db := openIntegrationDB(t)
	_, _, agent := seedClaimFixture(t, db)
	repo := NewRepository(db)

	got, err := repo.FindAgentByUUID(agent.AgentID)
	if err != nil || got.ID != agent.ID {
		t.Fatalf("按 uuid 查 agent 应命中: got=%v err=%v", got, err)
	}
	if _, err := repo.FindAgentByUUID(uuid.NewString()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("查无 agent 应返回 gorm.ErrRecordNotFound，实际 %v", err)
	}
}

// TestIntegrationClaimSkipsLockedRow SKIP LOCKED 行为级钉子（day32 评审 I-1）：
// 并发测试只能证明"恰 1 成功"（READ COMMITTED 下阻塞式 FOR UPDATE 结果相同）；
// 本测试钉住特性本身——会话 A 事务内 FOR UPDATE 锁住唯一 PENDING 行不提交，
// 会话 B claim 必须**立即**跳过被锁行返回 ErrNoTask；若实现退化为阻塞 FOR UPDATE，
// B 将挂在行锁上，2s 超时即判失败（特征测试：双向验证过——删 Options 变红，恢复变绿）。
func TestIntegrationClaimSkipsLockedRow(t *testing.T) {
	db := openIntegrationDB(t)
	_, _, agent := seedClaimFixture(t, db)
	repo := NewRepository(db)

	txA := db.Begin()
	defer txA.Rollback()
	var lockedID int64
	if err := txA.Raw("SELECT id FROM test_task WHERE status = ? FOR UPDATE", string(StatePending)).Scan(&lockedID).Error; err != nil {
		t.Fatalf("会话 A 锁行: %v", err)
	}
	if lockedID == 0 {
		t.Fatal("会话 A 未锁到行（fixture 异常）")
	}

	done := make(chan error, 1)
	go func() {
		_, _, _, err := repo.ClaimNextTask(agent.ID)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoTask) {
			t.Fatalf("被锁行应被跳过返回 ErrNoTask，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claim 阻塞在行锁上——SKIP LOCKED 未生效（退化为 FOR UPDATE）")
	}
}

// TestIntegrationClaimConcurrent 并发原子性（Review Focus 1，验收原文）：
// 10 goroutine 同抢 1 个 PENDING → 恰 1 成功、9 个 ErrNoTask；execution 恰 1 行且 attempt=1。
func TestIntegrationClaimConcurrent(t *testing.T) {
	db := openIntegrationDB(t)
	_, taskRow, agent := seedClaimFixture(t, db)
	repo := NewRepository(db)

	const n = 10
	var wg sync.WaitGroup
	type outcome struct {
		err    error
		execID string
	}
	outcomes := make(chan outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, exec, _, err := repo.ClaimNextTask(agent.ID)
			oc := outcome{err: err}
			if exec != nil {
				oc.execID = exec.ExecutionID
			}
			outcomes <- oc
		}()
	}
	wg.Wait()
	close(outcomes)

	var okCount, noTaskCount, execUUIDs = 0, 0, map[string]bool{}
	for oc := range outcomes {
		switch {
		case oc.err == nil:
			okCount++
			execUUIDs[oc.execID] = true
		case errors.Is(oc.err, ErrNoTask):
			noTaskCount++
		default:
			t.Errorf("并发领取出现意外错误: %v", oc.err)
		}
	}
	if okCount != 1 || noTaskCount != n-1 {
		t.Fatalf("应恰 1 成功 %d 个 ErrNoTask，实际 ok=%d noTask=%d", n-1, okCount, noTaskCount)
	}
	if len(execUUIDs) != 1 {
		t.Errorf("成功者应领到同一 execution，实际 %d 个不同 uuid", len(execUUIDs))
	}
	var execCount int64
	db.Model(&model.TestExecution{}).Where("task_id = ?", taskRow.ID).Count(&execCount)
	if execCount != 1 {
		t.Fatalf("DB execution 应恰 1 行，实际 %d", execCount)
	}
	var taskDB model.TestTask
	db.First(&taskDB, taskRow.ID)
	if taskDB.Status != "DISPATCHED" {
		t.Fatalf("任务最终应 DISPATCHED，实际 %s", taskDB.Status)
	}
}
