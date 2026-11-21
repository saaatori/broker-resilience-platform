package task

import (
	"errors"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// newEventFixture events 造数：agent(id=7) + DISPATCHED task(id=42) + CLAIMED exec(eid-1, task 42)
// + 真 EventsValidator + fake 闸门 + Hub（订阅 42 观察广播）。
func newEventFixture(t *testing.T) (*Service, *fakeTaskRepo, *fakeEventGate, *Hub, *model.AgentInstance, <-chan SSEEvent) {
	t.Helper()
	repo := newFakeTaskRepo()
	gate := &fakeEventGate{seen: map[string]bool{}, cancelSet: map[string]bool{}}
	hub := NewHub()
	ch, cancel := hub.Subscribe(42)
	t.Cleanup(cancel)
	repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, ScenarioID: 1, Status: "DISPATCHED"})
	repo.execRows = []model.TestExecution{{ID: 1, ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Status: "CLAIMED"}}
	agent := &model.AgentInstance{ID: 7, AgentID: "a-uuid", Status: "online"}
	svc := NewService(repo, ServiceDeps{Events: newTestEventsValidator(t), Gate: gate, Hub: hub})
	return svc, repo, gate, hub, agent, ch
}

const statusEvent = `{"seq":0,"ts":"2026-11-21T10:00:00+00:00","type":"STATUS","payload":{"substate":"PREPARING"}}`
const logEvent = `{"seq":1,"ts":"2026-11-21T10:00:01+00:00","type":"LOG","payload":{"message":"m"}}`
const progressEvent = `{"seq":2,"ts":"2026-11-21T10:00:02+00:00","type":"PROGRESS","payload":{"current":1,"total":10}}`
const mismatchEvent = `{"seq":3,"ts":"2026-11-21T10:00:03+00:00","type":"STATUS","payload":{"message":"x"}}`
const noTzEvent = `{"seq":4,"ts":"2026-11-21T10:00:04","type":"LOG","payload":{"message":"m"}}`

func TestEvent(t *testing.T) {
	t.Run("eid 不存在→404 哨兵", func(t *testing.T) {
		svc, _, _, _, agent, _ := newEventFixture(t)
		if err := svc.Event(agent, "no-such-eid", []byte(statusEvent)); !errors.Is(err, ErrExecutionNotFound) {
			t.Fatalf("want ErrExecutionNotFound, got %v", err)
		}
	})
	t.Run("他人 agent 的 eid→同一 404 哨兵（不泄漏归属）", func(t *testing.T) {
		svc, _, _, _, _, _ := newEventFixture(t)
		other := &model.AgentInstance{ID: 999, AgentID: "other"}
		if err := svc.Event(other, "eid-1", []byte(statusEvent)); !errors.Is(err, ErrExecutionNotFound) {
			t.Fatalf("want ErrExecutionNotFound, got %v", err)
		}
	})
	t.Run("schema 错配→ErrInvalidEvent", func(t *testing.T) {
		svc, _, _, _, agent, _ := newEventFixture(t)
		if err := svc.Event(agent, "eid-1", []byte(mismatchEvent)); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("want ErrInvalidEvent, got %v", err)
		}
	})
	t.Run("ts 无时区→ErrInvalidEvent", func(t *testing.T) {
		svc, _, _, _, agent, _ := newEventFixture(t)
		if err := svc.Event(agent, "eid-1", []byte(noTzEvent)); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("want ErrInvalidEvent, got %v", err)
		}
	})
	t.Run("重复 seq→nil 且无任何副作用", func(t *testing.T) {
		svc, repo, gate, _, agent, ch := newEventFixture(t)
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("首发: %v", err)
		}
		drain(ch)
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("重放应 nil: %v", err)
		}
		if len(ch) != 0 {
			t.Error("重放不应再广播")
		}
		if repo.tasks[0].Status != "RUNNING" {
			t.Errorf("重放不应二次转移: %s", repo.tasks[0].Status)
		}
		_ = gate
	})
	t.Run("DISPATCHED+首条 STATUS→RUNNING+子状态+双广播", func(t *testing.T) {
		svc, repo, _, _, agent, ch := newEventFixture(t)
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("Event: %v", err)
		}
		if repo.tasks[0].Status != "RUNNING" {
			t.Errorf("task 应转 RUNNING: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != "PREPARING" {
			t.Errorf("exec 子状态应 PREPARING: %s", repo.execRows[0].Status)
		}
		if len(ch) != 2 { // 透传 + task_status
			t.Fatalf("应广播两事件（透传+RUNNING），实际 %d", len(ch))
		}
	})
	t.Run("取消标志优先→CANCELLING", func(t *testing.T) {
		svc, repo, gate, _, agent, _ := newEventFixture(t)
		gate.cancelSet["eid-1"] = true
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("Event: %v", err)
		}
		if repo.tasks[0].Status != "CANCELLING" {
			t.Fatalf("取消标志应优先转 CANCELLING: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != "PREPARING" {
			t.Error("子状态仍应更新")
		}
	})
	t.Run("CANCELLING 转移广播 task 事件", func(t *testing.T) {
		svc, repo, gate, _, agent, ch := newEventFixture(t)
		gate.cancelSet["eid-1"] = true
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("Event: %v", err)
		}
		if repo.tasks[0].Status != "CANCELLING" {
			t.Fatalf("应转 CANCELLING: %s", repo.tasks[0].Status)
		}
		found := false
		n := len(ch) // 快照：接收会使 len 递减，动态上限会漏收
		for i := 0; i < n; i++ {
			if ev, ok := <-ch; ok && ev.Name == "task" {
				found = true
			}
		}
		if !found {
			t.Fatal("CANCELLING 转移应广播 task 事件")
		}
	})
	t.Run("非 JSON 请求体→ErrInvalidEvent", func(t *testing.T) {
		svc, _, _, _, agent, _ := newEventFixture(t)
		if err := svc.Event(agent, "eid-1", []byte("not-json")); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("want ErrInvalidEvent, got %v", err)
		}
	})
	t.Run("RUNNING+后续 STATUS→不转移仍透传+子状态更新", func(t *testing.T) {
		svc, repo, _, _, agent, ch := newEventFixture(t)
		repo.tasks[0].Status = "RUNNING"
		if err := svc.Event(agent, "eid-1", []byte(statusEvent)); err != nil {
			t.Fatalf("Event: %v", err)
		}
		if repo.tasks[0].Status != "RUNNING" {
			t.Errorf("不应转移: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != "PREPARING" {
			t.Error("子状态应更新")
		}
		if len(ch) != 1 { // 仅透传
			t.Fatalf("应只广播透传，实际 %d", len(ch))
		}
	})
	t.Run("终态×STATUS/LOG→不改态不转移（202 记录）", func(t *testing.T) {
		for _, fin := range []string{"COMPLETED", "FAILED", "CANCELLED"} {
			svc, repo, _, _, agent, ch := newEventFixture(t)
			repo.tasks[0].Status = fin
			for _, ev := range []string{statusEvent, logEvent} {
				if err := svc.Event(agent, "eid-1", []byte(ev)); err != nil {
					t.Fatalf("终态 %s 收事件应 nil: %v", fin, err)
				}
			}
			if repo.tasks[0].Status != fin {
				t.Errorf("终态 %s 不应改变: %s", fin, repo.tasks[0].Status)
			}
			if repo.execRows[0].Status != "CLAIMED" {
				t.Errorf("终态后子状态不应被覆盖: %s", repo.execRows[0].Status)
			}
			if len(ch) != 0 {
				t.Errorf("终态后不广播（前端已收终态），实际 %d", len(ch))
			}
		}
	})
	t.Run("LOG/PROGRESS 于 DISPATCHED→不转移仅透传", func(t *testing.T) {
		svc, repo, _, _, agent, ch := newEventFixture(t)
		for _, ev := range []string{logEvent, progressEvent} {
			if err := svc.Event(agent, "eid-1", []byte(ev)); err != nil {
				t.Fatalf("Event: %v", err)
			}
		}
		if repo.tasks[0].Status != "DISPATCHED" {
			t.Errorf("LOG/PROGRESS 不应触发转移: %s", repo.tasks[0].Status)
		}
		if len(ch) != 2 {
			t.Fatalf("应各透传一次，实际 %d", len(ch))
		}
	})
	t.Run("闸门 Redis 故障→错误上抛（非 404 非 400）", func(t *testing.T) {
		svc, _, gate, _, agent, _ := newEventFixture(t)
		gate.failErr = errors.New("redis down")
		err := svc.Event(agent, "eid-1", []byte(logEvent))
		if err == nil || errors.Is(err, ErrExecutionNotFound) || errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("Redis 故障应上抛（5xx 路径），实际 %v", err)
		}
	})
}

// drain 清空通道。
func drain(ch <-chan SSEEvent) {
	for len(ch) > 0 {
		<-ch
	}
}
