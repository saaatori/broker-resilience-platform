package task

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// 领取哨兵（产生于 repo 层，service/handler 共用）。
var (
	// ErrNoTask 无 PENDING 任务（含 SKIP LOCKED 跳过被锁行后为空——正常路径失败方，handler 204）。
	ErrNoTask = errors.New("no pending task")
	// ErrClaimConflict 领取冲突（协议 §2.3 防御性保留：状态更新 RowsAffected=0 等异常竞态，handler 409）。
	ErrClaimConflict = errors.New("claim conflict")
)

// LeaseTTL 租约时长（实施规划决策 3：45s 无心跳即判过期——数值口径冻结不得改；
// 心跳 15s 由 day33 heartbeat 端点刷新本值）。
const LeaseTTL = 45 * time.Second

// Repository 数据访问层：只封装 GORM 操作，不含业务规则。
type Repository struct {
	db *gorm.DB
}

// NewRepository 注入 *gorm.DB 构造 repository。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Create 写入一个测试任务（status 由 Service 置 PENDING）。
func (r *Repository) Create(t *model.TestTask) error {
	return r.db.Create(t).Error
}

// FindScenarioByKey 按场景键查询且仅返回启用场景（接口契约：禁用=不存在，建任务侧不可见）。
func (r *Repository) FindScenarioByKey(key string) (*model.TestScenario, error) {
	var sc model.TestScenario
	if err := r.db.Where("key = ? AND enabled = ?", key, true).First(&sc).Error; err != nil {
		return nil, err
	}
	return &sc, nil
}

// EnsureScenario 场景不存在则插入、存在即跳过（启动 seed 幂等）。
func (r *Repository) EnsureScenario(s *model.TestScenario) error {
	var count int64
	if err := r.db.Model(&model.TestScenario{}).Where("key = ?", s.Key).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return r.db.Create(s).Error
}

// FindAgentByUUID 按协议 agent_id(uuid) 查 agent_instance 行（claim 身份解析）。
func (r *Repository) FindAgentByUUID(agentUUID string) (*model.AgentInstance, error) {
	var a model.AgentInstance
	if err := r.db.Where("agent_id = ?", agentUUID).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// ClaimNextTask 原子领取一个 PENDING 任务（协议 v1.0.5 §2.3，状态机转移表行 1）。
// 单事务：FOR UPDATE SKIP LOCKED 锁最老 PENDING → 无行 ErrNoTask →
// attempt=COALESCE(MAX,0)+1 → INSERT execution（CLAIMED / 45s 租约 / started_at）→
// task 置 DISPATCHED → 查 scenario_key。
// 并发下同任务只有一个领取成功：SKIP LOCKED 使后来者跳过被锁行，落空即 ErrNoTask（204）。
// 事务任一步失败整体回滚——execution 不产生孤儿行、task 不误置 DISPATCHED（Review Focus 2）。
func (r *Repository) ClaimNextTask(agentRowID int64) (*model.TestTask, *model.TestExecution, string, error) {
	var task model.TestTask
	var exec model.TestExecution
	var scenarioKey string
	err := r.db.Transaction(func(tx *gorm.DB) error {
		// First 自带主键升序（=最老 PENDING 先领）；显式 Order("id") 会与之叠加成双重排序列。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", TaskStatusPENDING).
			First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNoTask
			}
			return fmt.Errorf("lock pending task: %w", err)
		}

		var maxAttempt int
		if err := tx.Raw("SELECT COALESCE(MAX(attempt), 0) FROM test_execution WHERE task_id = ?", task.ID).Scan(&maxAttempt).Error; err != nil {
			return fmt.Errorf("query max attempt: %w", err)
		}
		now := time.Now()
		leaseUntil := now.Add(LeaseTTL)
		exec = model.TestExecution{
			ExecutionID: uuid.NewString(),
			TaskID:      task.ID,
			Attempt:     maxAttempt + 1,
			AgentID:     agentRowID,
			Status:      "CLAIMED", // 协议 §5 执行子状态序列首值
			LeaseUntil:  &leaseUntil,
			StartedAt:   &now,
		}
		if err := tx.Create(&exec).Error; err != nil {
			return fmt.Errorf("create execution: %w", err)
		}

		res := tx.Model(&model.TestTask{}).Where("id = ?", task.ID).Update("status", "DISPATCHED")
		if res.Error != nil {
			return fmt.Errorf("dispatch task: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// 行已被 FOR UPDATE 锁定，理论不可达——防御性保留（协议 409 语义）。
			return fmt.Errorf("dispatch task %d: %w", task.ID, ErrClaimConflict)
		}
		task.Status = "DISPATCHED"

		// scenario 行缺失（外键孤儿数据）不静默降级为空 key——整体失败回滚，走 500+日志（day32 评审 M-3）。
		if err := tx.Raw("SELECT key FROM test_scenario WHERE id = ?", task.ScenarioID).Scan(&scenarioKey).Error; err != nil {
			return fmt.Errorf("query scenario key: %w", err)
		}
		if scenarioKey == "" {
			return fmt.Errorf("task %d 引用的 scenario %d 不存在（孤儿外键）", task.ID, task.ScenarioID)
		}
		return nil
	})
	if err != nil {
		return nil, nil, "", err
	}
	return &task, &exec, scenarioKey, nil
}
