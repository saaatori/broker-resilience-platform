package task

import (
	"errors"
	"testing"
	"time"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

var errFailingCache = errors.New("cache down")

var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// mkView 构造判定视图。
func mkView(execStatus string, agentRow int64, leaseUntil *time.Time, taskStatus string) execView {
	return execView{
		Exec:       model.TestExecution{ExecutionID: "e-1", AgentID: agentRow, Status: execStatus, LeaseUntil: leaseUntil},
		TaskStatus: taskStatus,
	}
}

func afterMin(d time.Duration) *time.Time { t := testNow.Add(d); return &t }

// TestJudgeExec v1.0.5 §2.2 lease_ok 判定分支序（Review Focus 1）：
// 归属→终态→租约（失效一律排除含 CANCELLING）→互斥取消优先。
func TestJudgeExec(t *testing.T) {
	cases := []struct {
		name string
		v    execView
		want execVerdict
	}{
		{"正常执行（CLAIMED/DISPATCHED 态/租约有效）", mkView("CLAIMED", 7, afterMin(30*time.Second), "DISPATCHED"), execLeaseOK},
		{"RUNNING 态同样认可", mkView("BASELINE", 7, afterMin(30*time.Second), "RUNNING"), execLeaseOK},
		{"归属他 Agent", mkView("CLAIMED", 999, afterMin(30*time.Second), "RUNNING"), execExclude},
		{"执行终态 DONE", mkView("DONE", 7, afterMin(30*time.Second), "RUNNING"), execExclude},
		{"执行终态 ABORTED", mkView("ABORTED", 7, afterMin(30*time.Second), "RUNNING"), execExclude},
		{"租约已过期", mkView("CLAIMED", 7, afterMin(-1*time.Second), "RUNNING"), execExclude},
		{"租约恰好等于当前时刻（After 严格判定=失效）", mkView("CLAIMED", 7, afterMin(0), "RUNNING"), execExclude},
		{"过期且任务 CANCELLING（v1.0.5：租约失效一律不入两数组）", mkView("CLAIMED", 7, afterMin(-1*time.Second), "CANCELLING"), execExclude},
		{"有效且任务 CANCELLING（互斥取消优先）", mkView("CLAIMED", 7, afterMin(30*time.Second), "CANCELLING"), execCancel},
		{"租约为 nil（防御）", mkView("CLAIMED", 7, nil, "RUNNING"), execExclude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeExec(tc.v, 7, testNow); got != tc.want {
				t.Fatalf("want %v got %v", tc.want, got)
			}
		})
	}
}

// newHBFixture 心跳 fake 造数：agent 行 + 三种执行视图。
func newHBFixture(t *testing.T) (*Service, *fakeTaskRepo, *model.AgentInstance) {
	t.Helper()
	repo := newFakeTaskRepo()
	agent := &model.AgentInstance{ID: 7, AgentID: "a-uuid", Hostname: "hb-host", TokenHash: "h", Status: "online"}
	repo.agents[agent.AgentID] = agent // 直接放入（绕过 UpsertAgent 以钉死 ID=7）
	future := testNow.Add(time.Hour)
	repo.execViews = []execView{
		{Exec: model.TestExecution{ExecutionID: "eid-ok", AgentID: 7, Status: "CLAIMED", LeaseUntil: &future}, TaskStatus: "RUNNING"},
		{Exec: model.TestExecution{ExecutionID: "eid-cancel", AgentID: 7, Status: "CLAIMED", LeaseUntil: &future}, TaskStatus: "CANCELLING"},
		{Exec: model.TestExecution{ExecutionID: "eid-other", AgentID: 999, Status: "CLAIMED", LeaseUntil: &future}, TaskStatus: "RUNNING"},
	}
	svc := NewService(repo, nil, nil, nil, nil)
	return svc, repo, agent
}

func TestHeartbeat(t *testing.T) {
	t.Run("三组混合→双数组正确且去重", func(t *testing.T) {
		svc, repo, agent := newHBFixture(t)
		leaseOK, cancel, err := svc.Heartbeat(agent, []string{"eid-ok", "eid-ok", "eid-cancel", "eid-other", "eid-unknown"})
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if len(leaseOK) != 1 || leaseOK[0] != "eid-ok" {
			t.Errorf("lease_ok 应恰 [eid-ok]（去重+他人+未知排除），实际 %v", leaseOK)
		}
		if len(cancel) != 1 || cancel[0] != "eid-cancel" {
			t.Errorf("cancel_requested 应恰 [eid-cancel]，实际 %v", cancel)
		}
		if len(repo.renewedEIDs) != 1 || repo.renewedEIDs[0] != "eid-ok" {
			t.Errorf("刷新应只对 lease_ok（Review Focus 4），实际 %v", repo.renewedEIDs)
		}
		if !repo.agentTouched {
			t.Error("agent last_heartbeat_at 应被刷新")
		}
		if d := time.Until(repo.renewedUntil); d < 40*time.Second || d > 45*time.Second {
			t.Errorf("刷新幅度应为 45s 口径，实际 %v", d)
		}
	})
	t.Run("空上报列表→空数组不报错", func(t *testing.T) {
		svc, repo, agent := newHBFixture(t)
		leaseOK, cancel, err := svc.Heartbeat(agent, []string{})
		if err != nil || len(leaseOK) != 0 || len(cancel) != 0 {
			t.Fatalf("空上报应空数组: %v %v %v", leaseOK, cancel, err)
		}
		if !repo.agentTouched {
			t.Error("空上报也应刷新 agent 心跳（存活信号）")
		}
	})
	t.Run("Redis 缓存故障不阻断（告警继续）", func(t *testing.T) {
		svc, repo, agent := newHBFixture(t)
		svc.hbCache = failingHBCache{}
		if _, _, err := svc.Heartbeat(agent, []string{"eid-ok"}); err != nil {
			t.Fatalf("缓存故障不应阻断心跳: %v", err)
		}
		if len(repo.renewedEIDs) != 1 {
			t.Error("租约刷新仍应发生（本体在 PG）")
		}
	})
}

// failingHBCache 永远失败的缓存（Review Focus 5）。
type failingHBCache struct{}

func (failingHBCache) SetHeartbeat(string) error { return errFailingCache }
