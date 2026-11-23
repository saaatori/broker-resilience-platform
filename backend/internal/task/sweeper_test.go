package task

import (
	"errors"
	"testing"
	"time"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// fakeCleanupQueue 清理队列内存实现。
type fakeCleanupQueue struct {
	enqueued []string
	failErr  error
}

func (f *fakeCleanupQueue) Enqueue(eid string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.enqueued = append(f.enqueued, eid)
	return nil
}

// newSweepFixture 清扫造数：hub 订阅 task=42 + 可注入过期视图。
func newSweepFixture(t *testing.T) (*Service, *fakeTaskRepo, *fakeCleanupQueue, *Hub, <-chan SSEEvent) {
	t.Helper()
	repo := newFakeTaskRepo()
	queue := &fakeCleanupQueue{}
	hub := NewHub()
	ch, cancel := hub.Subscribe(42)
	t.Cleanup(cancel)
	svc := NewService(repo, ServiceDeps{Gate: &fakeEventGate{seen: map[string]bool{}, cancelSet: map[string]bool{}},
		Hub: hub, CleanupQueue: queue})
	return svc, repo, queue, hub, ch
}

// mkExpiredView 构造一条扫描视图（exec + 任务态）。
func mkExpiredView(eid string, execStatus, taskStatus string) execView {
	past := time.Now().Add(-time.Minute)
	return execView{Exec: model.TestExecution{ExecutionID: eid, TaskID: 42, AgentID: 7, Status: execStatus, LeaseUntil: &past}, TaskStatus: taskStatus}
}

func TestSweepOnce(t *testing.T) {
	t.Run("DISPATCHED+过期→全链收口", func(t *testing.T) {
		svc, repo, queue, _, ch := newSweepFixture(t)
		repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, Status: string(StateDispatched)})
		repo.execRows = []model.TestExecution{{ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Status: "CLAIMED",
			LeaseUntil: ptrTime(time.Now().Add(-time.Minute))}}
		repo.sweepViews = []execView{mkExpiredView("eid-1", "CLAIMED", string(StateDispatched))}

		n, err := svc.SweepOnce(time.Now())
		if err != nil || n != 1 {
			t.Fatalf("SweepOnce: n=%d err=%v", n, err)
		}
		if repo.tasks[0].Status != string(StateFailed) {
			t.Errorf("task 应 FAILED: %s", repo.tasks[0].Status)
		}
		exec := &repo.execRows[0]
		if exec.Status != string(execStatusAborted) || exec.ExitReason == nil || *exec.ExitReason != "lease_lost" || exec.FinishedAt == nil {
			t.Errorf("exec 应 ABORTED/lease_lost/finished_at: %+v", exec)
		}
		if len(queue.enqueued) != 1 || queue.enqueued[0] != "eid-1" {
			t.Errorf("应入队一次: %v", queue.enqueued)
		}
		if len(ch) == 0 {
			t.Error("终态应广播")
		}
	})
	t.Run("CANCELLING+过期→FAILED（决策表行 10）", func(t *testing.T) {
		svc, repo, queue, _, _ := newSweepFixture(t)
		repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, Status: string(StateCancelling)})
		repo.sweepViews = []execView{mkExpiredView("eid-1", "CLAIMED", string(StateCancelling))}
		if n, err := svc.SweepOnce(time.Now()); err != nil || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if repo.tasks[0].Status != string(StateFailed) {
			t.Fatalf("CANCELLING 过期应 FAILED: %s", repo.tasks[0].Status)
		}
		if len(queue.enqueued) != 1 {
			t.Error("取消路径同样入队")
		}
	})
	t.Run("任务终态+过期→不扫", func(t *testing.T) {
		svc, repo, _, _, _ := newSweepFixture(t)
		repo.sweepViews = nil // 终态不入扫描集（repo 层过滤——fake 直接空）
		if n, _ := svc.SweepOnce(time.Now()); n != 0 {
			t.Fatalf("终态不扫: n=%d", n)
		}
	})
	t.Run("中断窗口：exec DONE+任务 RUNNING+过期→任务兜底 FAILED 但 exec 保持 DONE", func(t *testing.T) {
		svc, repo, _, _, _ := newSweepFixture(t)
		repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, Status: string(StateRunning)})
		doneExec := model.TestExecution{ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Status: string(execStatusDone),
			LeaseUntil: ptrTime(time.Now().Add(-time.Minute))}
		repo.execRows = []model.TestExecution{doneExec}
		repo.sweepViews = []execView{mkExpiredView("eid-1", string(execStatusDone), string(StateRunning))}

		if n, err := svc.SweepOnce(time.Now()); err != nil || n != 1 {
			t.Fatalf("中断窗口应兜底扫: n=%d err=%v", n, err)
		}
		if repo.tasks[0].Status != string(StateFailed) {
			t.Fatalf("任务应兜口 FAILED: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != string(execStatusDone) {
			t.Fatalf("exec DONE 不得覆盖 ABORTED（result 语义保全）: %s", repo.execRows[0].Status)
		}
	})
	t.Run("CAS 不中（并发已被转）→跳过不入队", func(t *testing.T) {
		svc, repo, queue, _, _ := newSweepFixture(t)
		// 扫描视图快照 DISPATCHED，但 fake 当前任务已 COMPLETED（result 抢先）
		repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, Status: string(StateCompleted)})
		repo.sweepViews = []execView{mkExpiredView("eid-1", "CLAIMED", string(StateDispatched))}
		if n, err := svc.SweepOnce(time.Now()); err != nil || n != 0 {
			t.Fatalf("CAS 不中应跳过: n=%d err=%v", n, err)
		}
		if repo.tasks[0].Status != string(StateCompleted) {
			t.Fatal("状态不得被覆盖")
		}
		if len(queue.enqueued) != 0 {
			t.Error("CAS 不中不入队（同 eid 不重复入队防御）")
		}
	})
	t.Run("队列写失败不阻断状态收口", func(t *testing.T) {
		svc, repo, queue, _, _ := newSweepFixture(t)
		queue.failErr = errors.New("redis down")
		repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, Status: string(StateRunning)})
		repo.execRows = []model.TestExecution{{ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Status: "CLAIMED",
			LeaseUntil: ptrTime(time.Now().Add(-time.Minute))}}
		repo.sweepViews = []execView{mkExpiredView("eid-1", "CLAIMED", string(StateRunning))}
		if _, err := svc.SweepOnce(time.Now()); err != nil {
			t.Fatalf("队列失败不应阻断: %v", err)
		}
		if repo.tasks[0].Status != string(StateFailed) {
			t.Fatal("状态收口应完成")
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }
