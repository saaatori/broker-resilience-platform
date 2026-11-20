package task

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// fakeTaskRepo taskRepository 的内存实现（单测用；GORM 哨兵错误保持与真实现一致）。
// ClaimNextTask 只模拟顺序语义（弹最老 PENDING）——并发原子性由集成测试在真库验证。
type fakeTaskRepo struct {
	tasks         []*model.TestTask
	scenarios     map[string]*model.TestScenario
	agents        map[string]*model.AgentInstance
	agentQueryErr error // 模拟 agent 查询 DB 故障（day31 I-1 同型分叉测试）
	nextID        int64
}

func newFakeTaskRepo() *fakeTaskRepo {
	return &fakeTaskRepo{scenarios: map[string]*model.TestScenario{}, agents: map[string]*model.AgentInstance{}, nextID: 1}
}

func (f *fakeTaskRepo) Create(t *model.TestTask) error {
	cp := *t
	cp.ID = f.nextID
	f.nextID++
	f.tasks = append(f.tasks, &cp)
	return nil
}

func (f *fakeTaskRepo) FindScenarioByKey(key string) (*model.TestScenario, error) {
	s, ok := f.scenarios[key]
	if !ok || !s.Enabled { // 接口契约：禁用场景视为不存在
		return nil, gorm.ErrRecordNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeTaskRepo) EnsureScenario(s *model.TestScenario) error {
	if _, ok := f.scenarios[s.Key]; ok {
		return nil
	}
	cp := *s
	cp.ID = f.nextID
	f.nextID++
	f.scenarios[s.Key] = &cp
	return nil
}

func (f *fakeTaskRepo) FindAgentByUUID(agentUUID string) (*model.AgentInstance, error) {
	if f.agentQueryErr != nil {
		return nil, f.agentQueryErr
	}
	a, ok := f.agents[agentUUID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return a, nil
}

func (f *fakeTaskRepo) ClaimNextTask(agentRowID int64) (*model.TestTask, *model.TestExecution, string, error) {
	for _, t := range f.tasks {
		if t.Status != TaskStatusPENDING {
			continue
		}
		key := ""
		for _, sc := range f.scenarios { // 按 ScenarioID 反查（与真实现语义一致，不硬编码 key）
			if sc.ID == t.ScenarioID {
				key = sc.Key
				break
			}
		}
		t.Status = "DISPATCHED"
		now := time.Now()
		lease := now.Add(LeaseTTL)
		exec := &model.TestExecution{
			ExecutionID: "00000000-0000-0000-0000-000000000001",
			TaskID:      t.ID,
			Attempt:     1,
			AgentID:     agentRowID,
			Status:      "CLAIMED",
			LeaseUntil:  &lease,
			StartedAt:   &now,
		}
		taskCopy := *t
		return &taskCopy, exec, key, nil
	}
	return nil, nil, "", ErrNoTask
}

// fakeInstanceRepo instanceFinder 内存实现。
type fakeInstanceRepo struct {
	instances map[int64]*model.BrokerInstance
}

func (f *fakeInstanceRepo) FindByID(id int64) (*model.BrokerInstance, error) {
	ins, ok := f.instances[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return ins, nil
}

// fakeUserRepo userFinder 内存实现（错误语义对齐 auth.UserRepository：
// 不存在返回 gorm.ErrRecordNotFound；failErr 模拟 DB 故障等非哨兵错误）。
type fakeUserRepo struct {
	users   map[string]*model.SysUser
	failErr error
}

func (f *fakeUserRepo) FindByUsername(username string) (*model.SysUser, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	u, ok := f.users[username]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return u, nil
}

// newTestService 装配内存依赖 + 真 Validator（embed 真编译，schema 层不 mock）。
func newTestService(t *testing.T, repo *fakeTaskRepo, instances map[int64]*model.BrokerInstance, users map[string]*model.SysUser) *Service {
	return NewService(repo, &fakeInstanceRepo{instances: instances}, &fakeUserRepo{users: users}, newTestValidator(t))
}
