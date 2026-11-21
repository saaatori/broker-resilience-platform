package task

import (
	"errors"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// newClaimFixture 内存造数：agent（uuid=dev-agent-uuid）+ 场景 + 1 个 PENDING 任务。
func newClaimFixture(t *testing.T) (*fixture, *model.AgentInstance, *model.TestTask) {
	t.Helper()
	f := &fixture{}
	repo := newFakeTaskRepo()
	agent := &model.AgentInstance{ID: 7, AgentID: "6f1c2d3e-0000-4000-8000-000000000001", Hostname: "dev", Version: "0.0.1", TokenHash: "x", Status: "online"}
	repo.agents[agent.AgentID] = agent
	repo.scenarios["normal_delivery"] = &model.TestScenario{ID: 1, Key: "normal_delivery", DefaultParams: "{}", Enabled: true}
	pending := &model.TestTask{ID: 42, ScenarioID: 1, InstanceID: 3, CreatedBy: 1, ParamsSnapshot: `{"message_count":1}`, Status: TaskStatusPENDING}
	repo.tasks = append(repo.tasks, pending)
	f.repo = repo
	f.svc = NewService(repo, ServiceDeps{}) // claim 路径不触 instance/user/validator
	return f, agent, pending
}

func TestClaim(t *testing.T) {
	t.Run("成功领取返回完整结果", func(t *testing.T) {
		f, agent, pending := newClaimFixture(t)
		res, err := f.svc.Claim(agent.AgentID)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if res.Task.ID != pending.ID || res.Task.Status != "DISPATCHED" {
			t.Errorf("task 不符: %+v", res.Task)
		}
		if res.Exec == nil || res.Exec.AgentID != agent.ID {
			t.Fatalf("execution.AgentID 应为内部 id %d，实际 %+v", agent.ID, res.Exec)
		}
		if res.ScenarioKey != "normal_delivery" {
			t.Errorf("scenario_key 应 normal_delivery，实际 %q", res.ScenarioKey)
		}
	})
	t.Run("agent 不存在返回哨兵", func(t *testing.T) {
		f, _, _ := newClaimFixture(t)
		if _, err := f.svc.Claim("00000000-0000-0000-0000-00000000dead"); !errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("应返回 ErrAgentNotFound，实际 %v", err)
		}
	})
	t.Run("无任务上抛 ErrNoTask", func(t *testing.T) {
		f, agent, _ := newClaimFixture(t)
		f.repo.tasks[0].Status = "DISPATCHED" // 清空 PENDING
		if _, err := f.svc.Claim(agent.AgentID); !errors.Is(err, ErrNoTask) {
			t.Fatalf("应上抛 ErrNoTask，实际 %v", err)
		}
	})
	t.Run("repo 查询故障保留底层原因", func(t *testing.T) {
		f, agent, _ := newClaimFixture(t)
		f.repo.agentQueryErr = errors.New("db down")
		if _, err := f.svc.Claim(agent.AgentID); err == nil || errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("DB 故障不应混为 ErrAgentNotFound，实际 %v", err)
		}
	})
}
