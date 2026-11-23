// Package task 任务创建（M2 第 31 天）。
// service.go：最小依赖接口声明（单测可换内存实现，instance 模块先例）+ Service 装配 + 场景 seed。
package task

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

//go:embed seed/normal_delivery.json
var seedFS embed.FS

// taskRepository Service 依赖的最小数据访问接口。
type taskRepository interface {
	Create(t *model.TestTask) error
	// FindScenarioByKey 只返回 enabled 的场景（禁用场景对建任务视为不存在）。
	FindScenarioByKey(key string) (*model.TestScenario, error)
	EnsureScenario(s *model.TestScenario) error
	// FindAgentByUUID claim 身份解析：协议 agent_id(uuid) → agent_instance 内部行（day32）。
	FindAgentByUUID(agentUUID string) (*model.AgentInstance, error)
	// ClaimNextTask 原子领取（SKIP LOCKED 事务；并发语义仅真库可验——单测 fake 只模拟顺序路径）。
	ClaimNextTask(agentRowID int64) (*model.TestTask, *model.TestExecution, string, error)
	// UpsertAgent 注册原子幂等（day33 评审 I-1）：ON CONFLICT 单语句，返回（复用的）agent uuid。
	UpsertAgent(agentUUID, hostname, version, tokenHash string, maxConcurrent int) (string, error)
	// FindByTokenHash X-Agent-Token 验证（day33）。
	FindByTokenHash(tokenHash string) (*model.AgentInstance, error)
	// Heartbeat 三方法（day33）：视图查询 / 批量租约刷新 / agent 存活。
	FindExecutionsWithTaskStatus(eids []string) ([]execView, error)
	RenewLeases(eids []string, until time.Time) error
	TouchAgentHeartbeat(rowID int64) error
	// Result 五方法（day35）：幂等查询 / 首落 / 指标展开 / artifacts 登记 / 执行终态。
	FindResultByEID(eid string) (*model.TestResult, error)
	CreateResult(r *model.TestResult) error
	SaveMetricSummaries(executionID string, metrics map[string]float64, units map[string]string) error
	SaveReportArtifacts(taskID int64, executionID string, artifacts []string) error
	FinishExecution(eid, substate, exitReason string) error
	// Events 四方法（day34）：归属查询 / 任务态查询 / CAS 转移 / 子状态更新。
	FindExecutionByEID(eid string) (*model.TestExecution, error)
	FindTaskByID(id int64) (*model.TestTask, error)
	// TransitionTaskStatus CAS 状态转移：仅当当前态=from 时置 to，返回是否发生转移（幂等）。
	TransitionTaskStatus(taskID int64, from, to string) (bool, error)
	UpdateExecSubstate(eid, substate string) error
}

// instanceFinder 查询被测实例是否存在（复用 instance.Repository，不重复建模）。
type instanceFinder interface {
	FindByID(id int64) (*model.BrokerInstance, error)
}

// userFinder JWT 用户名 → sys_user 行（created_by 落库需要 id）。
type userFinder interface {
	FindByUsername(username string) (*model.SysUser, error)
}

// ServiceDeps Service 的可选依赖集合（day34 收拢：NewService 参数不再逐个追加；
// 单测零值即可用——nil 依赖在被触达时才需要）。
type ServiceDeps struct {
	Instances instanceFinder
	Users     userFinder
	Validator *Validator
	HB        hbCache // nil=跳过 agent:hb 缓存键（单测）
	Events    *EventsValidator
	Result    *ResultValidator
	Gate      eventGate // nil=Event() 不可用（防误用 panic 由调用方装配保证）
	Hub       *Hub
}

// Service task 业务逻辑层。
type Service struct {
	repo taskRepository
	deps ServiceDeps
	// 保留快捷引用（Create/Claim/Register 热路径直取）
	instances instanceFinder
	users     userFinder
	validator *Validator
	hbCache   hbCache
}

// NewService 注入依赖构造 Service（deps 零值可用于部分路径单测）。
func NewService(repo taskRepository, deps ServiceDeps) *Service {
	return &Service{
		repo: repo, deps: deps,
		instances: deps.Instances, users: deps.Users, validator: deps.Validator, hbCache: deps.HB,
	}
}

// FindTask 任务查询（SSE 订阅存在性校验等 handler 面复用）。
func (s *Service) FindTask(id int64) (*model.TestTask, error) {
	return s.repo.FindTaskByID(id)
}

// SubscribeTaskEvents 前端 SSE 订阅入口（handler 经此触达 hub——deps 私有不外泄）。
func (s *Service) SubscribeTaskEvents(taskID int64) (<-chan SSEEvent, func()) {
	return s.deps.Hub.Subscribe(taskID)
}

// seedScenarioKey 内置场景键（= 场景库目录名，协议 §6.1 合成规则的默认参数来源）。
const seedScenarioKey = "normal_delivery"

// SeedScenario 启动时 seed 内置场景（main 装配调用，SeedAdmin 模式）：
// key 不存在则插入，存在即跳过——幂等，服务重启不重复不报错。
// default_params 的权威 = lab/scenarios/normal_delivery/params.default.json（day26 定稿），
// embed 副本防漂移钉子（seed_test）保证两者一致。
func (s *Service) SeedScenario() error {
	raw, err := seedFS.ReadFile("seed/normal_delivery.json")
	if err != nil {
		return fmt.Errorf("read embedded seed: %w", err)
	}
	sc := &model.TestScenario{
		Key:           seedScenarioKey,
		Name:          "正常投递消费",
		Description:   ptr("生产者控速发送 → 消费者手动 ACK → 对账五阶段计数（M1 第 22 天场景，万条验收基线）"),
		DefaultParams: string(raw),
		Enabled:       true,
	}
	if err := s.repo.EnsureScenario(sc); err != nil {
		return fmt.Errorf("ensure scenario %q: %w", seedScenarioKey, err)
	}
	log.Printf("task: 场景 seed 完成（%s）", seedScenarioKey)
	return nil
}

// ptr 便捷取指针（seed 描述等单处使用）。
func ptr[T any](v T) *T { return &v }

// 建任务错误哨兵（handler 按此映射 400 文案）。
var (
	ErrScenarioNotFound = fmt.Errorf("scenario not found or disabled")
	ErrInstanceNotFound = fmt.Errorf("instance not found")
	ErrUserNotFound     = fmt.Errorf("user not found")
	ErrInvalidParams    = fmt.Errorf("invalid params_snapshot")
)

// Create 创建测试任务：场景默认参数 + 用户顶层浅覆盖 + 平台注入 → schema 校验 → 落库 PENDING。
// 平台管理键不透传：task_id/execution_id 一律清除（claim 时由平台注入，协议 §2.3），
// instance_ref 由平台覆盖为 strconv 格式的实例 id（day31 裁决：与 task_id v1.0.5 字符串口径一致）。
func (s *Service) Create(scenarioKey string, instanceID int64, paramsOverride map[string]any, username string) (*model.TestTask, error) {
	sc, err := s.repo.FindScenarioByKey(scenarioKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrScenarioNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find scenario: %w", err)
	}

	if _, err := s.instances.FindByID(instanceID); errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInstanceNotFound
	} else if err != nil {
		return nil, fmt.Errorf("find instance: %w", err)
	}

	user, err := s.users.FindByUsername(username)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err) // DB 故障等：保留底层原因，走 handler 500+日志路径
	}

	snapshot, err := mergeSnapshot(sc.DefaultParams, paramsOverride, strconv.FormatInt(instanceID, 10))
	if err != nil {
		return nil, fmt.Errorf("merge snapshot: %w", err)
	}
	if err := s.validator.Validate([]byte(snapshot)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidParams, err)
	}

	task := &model.TestTask{
		ScenarioID:     sc.ID,
		InstanceID:     instanceID,
		CreatedBy:      int64(user.ID), // sys_user.id 为 M0 遗留 uint；表 19 created_by=bigint（int64），显式转换
		ParamsSnapshot: snapshot,
		Status:         string(StatePending),
	}
	if err := s.repo.Create(task); err != nil {
		return nil, fmt.Errorf("create test_task: %w", err)
	}
	return task, nil
}

// mergeSnapshot 合成参数快照串：默认参数 ← 覆盖体顶层浅合并 → 清平台管理键 → 注入 instance_ref。
// 默认参数 UseNumber 解码：定稿整数值不经 float64 往返（10000 输出 10000）。
// 覆盖体来自 HTTP 绑定（float64），域内数值（消息量/速率/秒数/率）marshal 无失真。
func mergeSnapshot(defaultParams string, override map[string]any, instanceRef string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(defaultParams))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return "", fmt.Errorf("decode default_params: %w", err)
	}
	for k, v := range override {
		m[k] = v
	}
	delete(m, "task_id")
	delete(m, "execution_id")
	m["instance_ref"] = instanceRef
	out, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot: %w", err)
	}
	return string(out), nil
}
