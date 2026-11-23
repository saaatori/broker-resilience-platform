package task

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm/logger"

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

// FindAgentByUUID 按协议 agent_id(uuid) 查 agent_instance 行（claim 身份解析；非 uuid 同归一 404 语义）。
func (r *Repository) FindAgentByUUID(agentUUID string) (*model.AgentInstance, error) {
	if _, err := uuid.Parse(agentUUID); err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	var a model.AgentInstance
	if err := r.db.Where("agent_id = ?", agentUUID).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// FindExecutionsWithTaskStatus 心跳判定视图：exec JOIN task.status 一次取齐（eids 批量）。
func (r *Repository) FindExecutionsWithTaskStatus(eids []string) ([]execView, error) {
	var views []execView
	if err := r.db.Table("test_execution AS e").
		Select("e.*, t.status AS task_status").
		Joins("JOIN test_task t ON t.id = e.task_id").
		Where("e.execution_id IN ?", eids).
		Find(&views).Error; err != nil {
		return nil, err
	}
	return views, nil
}

// RenewLeases 批量刷新租约（仅 lease_ok 的执行——取消路径不刷，裁决记档开发日志）。
func (r *Repository) RenewLeases(eids []string, until time.Time) error {
	return r.db.Model(&model.TestExecution{}).
		Where("execution_id IN ?", eids).
		Update("lease_until", until).Error
}

// FindResultByEID 结果幂等查询（uq(execution_id) first-wins 判定）。
func (r *Repository) FindResultByEID(eid string) (*model.TestResult, error) {
	var res model.TestResult
	if err := r.db.Where("execution_id = ?", eid).First(&res).Error; err != nil {
		return nil, err
	}
	return &res, nil
}

// CreateResult 首次落库；uq(execution_id) 冲突转 ErrResultExists（并发迟到方，service 吞为幂等 200）。
func (r *Repository) CreateResult(res *model.TestResult) error {
	if err := r.db.Create(res).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrResultExists
		}
		return err
	}
	return nil
}

// SaveMetricSummaries 五指标展开（表 23；uq(execution_id,metric_key) 冲突跳过——幂等防御）。
func (r *Repository) SaveMetricSummaries(executionID string, metrics map[string]float64, units map[string]string) error {
	rows := make([]model.TestMetricSummary, 0, len(metrics))
	for key, value := range metrics {
		rows = append(rows, model.TestMetricSummary{ExecutionID: executionID, MetricKey: key, Value: value, Unit: units[key]})
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, len(rows)).Error
}

// SaveReportArtifacts 每 artifact 一行 test_report（协议 §2.5：Go 侧按路径登记）。
func (r *Repository) SaveReportArtifacts(taskID int64, executionID string, artifacts []string) error {
	rows := make([]model.TestReport, 0, len(artifacts))
	for _, path := range artifacts {
		rows = append(rows, model.TestReport{TaskID: taskID, ExecutionID: executionID, FilePath: path, GeneratedAt: time.Now()})
	}
	return r.db.CreateInBatches(&rows, len(rows)).Error
}

// FinishExecution 执行收口：子状态置终态（day33 M-2）+ finished_at + exit_reason（表 20）。
func (r *Repository) FinishExecution(eid, substate, exitReason string) error {
	updates := map[string]any{"status": substate, "finished_at": time.Now()}
	if exitReason != "" {
		updates["exit_reason"] = exitReason // COMPLETED=空 / FAILED=error / CANCELLED=cancelled
	}
	// 终态保持（day37 评审 I-1）：先到者固化——sweep 与 result 竞态时后写方 RowsAffected=0 幂等放弃，
	// 杜绝 test_result=COMPLETED 而 exec=ABORTED 的写序互覆盖账实矛盾。
	return r.db.Model(&model.TestExecution{}).
		Where("execution_id = ? AND status NOT IN ?", eid, []string{string(execStatusDone), string(execStatusAborted)}).
		Updates(updates).Error
}

// FindLeaseExpired 清扫扫描集：e.lease_until < now 且任务非终态（DISPATCHED/RUNNING/CANCELLING；
// 中断窗口 exec DONE+任务未转 含内——day35 记档兜底）；表 25 idx(lease_until) 支撑。
func (r *Repository) FindLeaseExpired(now time.Time) ([]execView, error) {
	var views []execView
	// 周期空轮不打 SQL 日志（day37 评审 M-4：默认 45s 下约 1920 条/天噪音）——Session 级静默
	if err := r.db.Session(&gorm.Session{Logger: logger.Discard}).
		Table("test_execution AS e").
		Select("e.*, t.status AS task_status").
		Joins("JOIN test_task t ON t.id = e.task_id").
		Where("e.lease_until < ? AND t.status IN ?", now,
			[]string{string(StateDispatched), string(StateRunning), string(StateCancelling)}).
		Find(&views).Error; err != nil {
		return nil, err
	}
	return views, nil
}

// FindExecutionByEID events/result 归属查询（eid=对外 uuid）。
// 非 uuid 字符串前置归一为 RecordNotFound（uuid 列 22P02 输入错误会 500——协议 §1：不存在 eid 得 404）。
func (r *Repository) FindExecutionByEID(eid string) (*model.TestExecution, error) {
	if _, err := uuid.Parse(eid); err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	var e model.TestExecution
	if err := r.db.Where("execution_id = ?", eid).First(&e).Error; err != nil {
		return nil, err
	}
	return &e, nil
}

// FindTaskByID 任务态查询（events 终态兜底判定用）。
func (r *Repository) FindTaskByID(id int64) (*model.TestTask, error) {
	var t model.TestTask
	if err := r.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// TransitionTaskStatus CAS 状态转移（协议 §4）：仅当当前态=from 时置 to；
// RowsAffected=1 返回 true（发生转移），=0 返回 false（已转移/已终态——幂等安全）。
func (r *Repository) TransitionTaskStatus(taskID int64, from, to string) (bool, error) {
	res := r.db.Model(&model.TestTask{}).
		Where("id = ? AND status = ?", taskID, from).
		Update("status", to)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// UpdateExecSubstate STATUS 事件更新执行子状态（表 20："执行子状态（事件上报更新）"）。
func (r *Repository) UpdateExecSubstate(eid, substate string) error {
	return r.db.Model(&model.TestExecution{}).
		Where("execution_id = ?", eid).
		Update("status", substate).Error
}

// TouchAgentHeartbeat agent 存活信号：last_heartbeat_at=now + status=online（表 16）。
func (r *Repository) TouchAgentHeartbeat(rowID int64) error {
	return r.db.Model(&model.AgentInstance{}).Where("id = ?", rowID).
		Updates(map[string]any{"last_heartbeat_at": time.Now(), "status": agentStatusOnline}).Error
}
func (r *Repository) FindByTokenHash(tokenHash string) (*model.AgentInstance, error) {
	var a model.AgentInstance
	if err := r.db.Where("token_hash = ?", tokenHash).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// UpsertAgent 注册原子幂等（day33 评审 I-1 修复）：单语句 INSERT … ON CONFLICT (hostname)
// DO UPDATE + RETURNING agent_id——并发同 hostname 收敛为恰 1 行，复用既有 uuid；
// 冲突分支换发 token 哈希（旧 token 立即失效）+ 更新版本/并发度/在线态。
func (r *Repository) UpsertAgent(agentUUID, hostname, version, tokenHash string, maxConcurrent int) (string, error) {
	var reusedUUID string
	err := r.db.Raw(`
		INSERT INTO agent_instance (agent_id, hostname, version, token_hash, max_concurrent, status)
		VALUES (?, ?, ?, ?, ?, 'online')
		ON CONFLICT (hostname) DO UPDATE SET
			token_hash = EXCLUDED.token_hash,
			version = EXCLUDED.version,
			max_concurrent = EXCLUDED.max_concurrent,
			status = 'online'
		RETURNING agent_id`,
		agentUUID, hostname, version, tokenHash, maxConcurrent).Scan(&reusedUUID).Error
	if err != nil {
		return "", err
	}
	return reusedUUID, nil
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
			Where("status = ?", string(StatePending)).
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

		res := tx.Model(&model.TestTask{}).Where("id = ?", task.ID).Update("status", string(StateDispatched))
		if res.Error != nil {
			return fmt.Errorf("dispatch task: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// 行已被 FOR UPDATE 锁定，理论不可达——防御性保留（协议 409 语义）。
			return fmt.Errorf("dispatch task %d: %w", task.ID, ErrClaimConflict)
		}
		task.Status = string(StateDispatched)

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
