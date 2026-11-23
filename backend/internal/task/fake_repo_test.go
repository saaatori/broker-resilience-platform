package task

import (
	"errors"
	"fmt"
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
	execViews     []execView
	execRows      []model.TestExecution
	sweepViews    []execView
	resultRows    map[string]*model.TestResult
	summaryRows   []model.TestMetricSummary
	reportRows    []model.TestReport
	renewedEIDs   []string // RenewLeases 入参记录（断言"只刷 lease_ok"）
	renewedUntil  time.Time
	agentTouched  bool
	nextID        int64
}

func newFakeTaskRepo() *fakeTaskRepo {
	return &fakeTaskRepo{scenarios: map[string]*model.TestScenario{}, agents: map[string]*model.AgentInstance{}, resultRows: map[string]*model.TestResult{}, nextID: 1}
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

func (f *fakeTaskRepo) UpsertAgent(agentUUID, hostname, version, tokenHash string, maxConcurrent int) (string, error) {
	for _, a := range f.agents { // 已注册：复用 agent_id、换 token（与真 repo ON CONFLICT 语义一致）
		if a.Hostname == hostname {
			a.TokenHash = tokenHash
			a.Version = version
			a.MaxConcurrent = maxConcurrent
			a.Status = "online"
			return a.AgentID, nil
		}
	}
	row := &model.AgentInstance{ID: f.nextID, AgentID: agentUUID, Hostname: hostname, Version: version,
		TokenHash: tokenHash, MaxConcurrent: maxConcurrent, Status: "online"}
	f.nextID++
	f.agents[agentUUID] = row
	return agentUUID, nil
}

func (f *fakeTaskRepo) FindByTokenHash(tokenHash string) (*model.AgentInstance, error) {
	if f.agentQueryErr != nil {
		return nil, f.agentQueryErr
	}
	for _, a := range f.agents {
		if a.TokenHash == tokenHash {
			return a, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (f *fakeTaskRepo) FindExecutionsWithTaskStatus(eids []string) ([]execView, error) {
	set := map[string]bool{}
	for _, e := range eids {
		set[e] = true
	}
	var out []execView
	for _, v := range f.execViews {
		if set[v.Exec.ExecutionID] {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeTaskRepo) RenewLeases(eids []string, until time.Time) error {
	f.renewedEIDs = append(f.renewedEIDs, eids...)
	f.renewedUntil = until
	return nil
}

func (f *fakeTaskRepo) TouchAgentHeartbeat(rowID int64) error {
	f.agentTouched = true
	return nil
}

func (f *fakeTaskRepo) ClaimNextTask(agentRowID int64) (*model.TestTask, *model.TestExecution, string, error) {
	for _, t := range f.tasks {
		if t.Status != string(StatePending) {
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
	return NewService(repo, ServiceDeps{Instances: &fakeInstanceRepo{instances: instances}, Users: &fakeUserRepo{users: users}, Validator: newTestValidator(t)})
}

// fakeEventGate 事件闸门内存实现（seen 判重；cancelSet 取消标志；failErr 模拟 Redis 故障）。
type fakeEventGate struct {
	seen          map[string]bool
	cancelSet     map[string]bool
	removedCancel map[string]bool
	removeFail    bool
	failErr       error
}

func (f *fakeEventGate) TryClaimSeq(eid string, seq int64) (bool, error) {
	if f.failErr != nil {
		return false, f.failErr
	}
	key := fmt.Sprintf("%s:%d", eid, seq)
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

func (f *fakeEventGate) CancelFlag(eid string) (bool, error) {
	if f.failErr != nil {
		return false, f.failErr
	}
	return f.cancelSet[eid], nil
}

func (f *fakeEventGate) RemoveCancelFlag(eid string) error {
	if f.removeFail {
		return errors.New("redis del down")
	}
	f.removedCancel[eid] = true
	delete(f.cancelSet, eid)
	return nil
}

func (f *fakeTaskRepo) FindExecutionByEID(eid string) (*model.TestExecution, error) {
	for i := range f.execRows {
		if f.execRows[i].ExecutionID == eid {
			cp := f.execRows[i]
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (f *fakeTaskRepo) FindTaskByID(id int64) (*model.TestTask, error) {
	for _, t := range f.tasks {
		if t.ID == id {
			cp := *t
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

// TransitionTaskStatus CAS 语义：仅当前态=from 才置 to。
func (f *fakeTaskRepo) TransitionTaskStatus(taskID int64, from, to string) (bool, error) {
	for _, t := range f.tasks {
		if t.ID == taskID && t.Status == from {
			t.Status = to
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeTaskRepo) UpdateExecSubstate(eid, substate string) error {
	for i := range f.execRows {
		if f.execRows[i].ExecutionID == eid {
			f.execRows[i].Status = substate
			return nil
		}
	}
	return gorm.ErrRecordNotFound
}

func (f *fakeTaskRepo) FindLeaseExpired(now time.Time) ([]execView, error) {
	return f.sweepViews, nil // 命中集由测试注入（终态过滤等 repo 层语义由真库集成测试覆盖）
}

func (f *fakeTaskRepo) FindResultByEID(eid string) (*model.TestResult, error) {
	if r, ok := f.resultRows[eid]; ok {
		return r, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (f *fakeTaskRepo) CreateResult(r *model.TestResult) error {
	if _, ok := f.resultRows[r.ExecutionID]; ok {
		return ErrResultExists // 与真库契约一致（TranslateError 后 uq 冲突=ErrResultExists）
	}
	cp := *r
	f.resultRows[r.ExecutionID] = &cp
	return nil
}

func (f *fakeTaskRepo) SaveMetricSummaries(executionID string, metrics map[string]float64, units map[string]string) error {
	for k, v := range metrics {
		f.summaryRows = append(f.summaryRows, model.TestMetricSummary{ExecutionID: executionID, MetricKey: k, Value: v, Unit: units[k]})
	}
	return nil
}

func (f *fakeTaskRepo) SaveReportArtifacts(taskID int64, executionID string, artifacts []string) error {
	for _, a := range artifacts {
		f.reportRows = append(f.reportRows, model.TestReport{TaskID: taskID, ExecutionID: executionID, FilePath: a})
	}
	return nil
}

func (f *fakeTaskRepo) FinishExecution(eid, substate, exitReason string) error {
	for i := range f.execRows {
		if f.execRows[i].ExecutionID == eid {
			f.execRows[i].Status = substate
			f.execRows[i].ExitReason = &exitReason
			now := time.Now() // 与真 repo 对齐：finished_at 一并落
			f.execRows[i].FinishedAt = &now
			return nil
		}
	}
	return gorm.ErrRecordNotFound
}
