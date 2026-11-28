// cancel_test.go 用户取消端点单测（M2 第 41 天，协议 v1.0.5 I-2 / §4 表行 5）。
package task

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// cancelFixture 组装 Cancel 测试依赖：fake 仓 + fake 闸门 + 真 Hub（无订阅者时 Publish 为 no-op）。
type cancelFixture struct {
	repo *fakeTaskRepo
	gate *fakeEventGate
	svc  *Service
}

func newCancelFixture(t *testing.T) *cancelFixture {
	t.Helper()
	repo := newFakeTaskRepo()
	gate := &fakeEventGate{seen: map[string]bool{}, cancelSet: map[string]bool{}, removedCancel: map[string]bool{}}
	svc := NewService(repo, ServiceDeps{Gate: gate, Hub: NewHub()})
	return &cancelFixture{repo: repo, gate: gate, svc: svc}
}

func (f *cancelFixture) task(t *testing.T, id int64, status TaskState) *model.TestTask {
	t.Helper()
	task := &model.TestTask{ID: id, Status: string(status), ScenarioID: 1, CreatedBy: 1}
	f.repo.tasks = append(f.repo.tasks, task)
	return task
}

func (f *cancelFixture) execution(t *testing.T, taskID int64, eid string) {
	t.Helper()
	f.repo.execRows = append(f.repo.execRows, model.TestExecution{
		TaskID: taskID, ExecutionID: eid, Attempt: 1, Status: "CLAIMED",
		LeaseUntil: ptrTime(time.Now().Add(time.Minute)),
	})
}

func subscribedStatuses(t *testing.T, hub *Hub, taskID int64, act func()) []string {
	t.Helper()
	ch, cancel := hub.Subscribe(taskID)
	defer cancel()
	done := make(chan []string, 1)
	go func() {
		var got []string
		for ev := range ch {
			if ev.Name == "task" {
				if m, ok := ev.Data.(map[string]any); ok {
					if s, ok := m["status"].(string); ok {
						got = append(got, s)
					}
				}
			}
		}
		done <- got
	}()
	act()
	cancel()
	select {
	case got := <-done:
		return got
	case <-time.After(time.Second):
		t.Fatal("订阅协程未退出")
		return nil
	}
}

// ---------- Service.Cancel 五状态分派 ----------

func TestCancelPendingDirectToCancelled(t *testing.T) {
	f := newCancelFixture(t)
	f.task(t, 1, StatePending)

	hub := NewHub()
	f.svc.deps.Hub = hub
	statuses := subscribedStatuses(t, hub, 1, func() {
		if _, err := f.svc.Cancel(1); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	})

	if got := f.repo.tasks[0].Status; got != string(StateCancelled) {
		t.Fatalf("PENDING 取消后状态 = %q, want CANCELLED", got)
	}
	if len(statuses) != 1 || statuses[0] != "CANCELLED" {
		t.Fatalf("SSE 终态广播 = %v, want [CANCELLED]", statuses)
	}
}

func TestCancelDispatchedSetsFlagWithoutTransition(t *testing.T) {
	f := newCancelFixture(t)
	f.task(t, 2, StateDispatched)
	f.execution(t, 2, "eid-d1")

	if _, err := f.svc.Cancel(2); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if got := f.repo.tasks[0].Status; got != string(StateDispatched) {
		t.Fatalf("DISPATCHED 取消不得转移状态，得 %q", got)
	}
	if !f.gate.cancelSet["eid-d1"] {
		t.Fatal("exec:cancel 标志未写入")
	}
}

func TestCancelRunningTransitionsToCancelling(t *testing.T) {
	f := newCancelFixture(t)
	f.task(t, 3, StateRunning)
	f.execution(t, 3, "eid-r0") // 评审 M-1：有执行行才能钉住"RUNNING 不置标志"（FindActive 找得到才会走到变异路径）

	hub := NewHub()
	f.svc.deps.Hub = hub
	statuses := subscribedStatuses(t, hub, 3, func() {
		if _, err := f.svc.Cancel(3); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	})

	if got := f.repo.tasks[0].Status; got != string(StateCancelling) {
		t.Fatalf("RUNNING 取消后状态 = %q, want CANCELLING", got)
	}
	if f.gate.setCalled > 0 {
		t.Fatal("RUNNING 取消不应置标志（judgeExec 走任务态 CANCELLING）")
	}
	if len(statuses) != 1 || statuses[0] != "CANCELLING" {
		t.Fatalf("SSE 广播 = %v, want [CANCELLING]", statuses)
	}
}

func TestCancelCancellingIsIdempotent(t *testing.T) {
	f := newCancelFixture(t)
	f.task(t, 4, StateCancelling)
	f.execution(t, 4, "eid-c1")

	if _, err := f.svc.Cancel(4); err != nil {
		t.Fatalf("重复取消应幂等 200: %v", err)
	}
	if got := f.repo.tasks[0].Status; got != string(StateCancelling) {
		t.Fatalf("状态被意外改动: %q", got)
	}
}

func TestCancelTerminalStatesConflict(t *testing.T) {
	for _, status := range []TaskState{StateCompleted, StateFailed, StateCancelled} {
		f := newCancelFixture(t)
		f.task(t, 5, status)
		if _, err := f.svc.Cancel(5); !errors.Is(err, ErrCancelConflict) {
			t.Fatalf("%s 取消应 ErrCancelConflict, got %v", status, err)
		}
	}
}

func TestCancelTaskNotFound(t *testing.T) {
	f := newCancelFixture(t)
	if _, err := f.svc.Cancel(99); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("应返回 ErrRecordNotFound, got %v", err)
	}
}

// ---------- handler ----------

func TestCancelHandlerShapes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newReq := func(body string) (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "7"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/tasks/7/cancel", strings.NewReader(body))
		return c, w
	}

	t.Run("200 返回取消后状态", func(t *testing.T) {
		f := newCancelFixture(t)
		f.task(t, 7, StateRunning)
		h := NewHandler(f.svc)
		c, w := newReq("")
		h.cancel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应体: %v", err)
		}
		if resp["status"] != "CANCELLING" {
			t.Fatalf("status = %v, want CANCELLING", resp["status"])
		}
	})

	t.Run("终态 409", func(t *testing.T) {
		f := newCancelFixture(t)
		f.task(t, 7, StateCompleted)
		h := NewHandler(f.svc)
		c, w := newReq("")
		h.cancel(c)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409", w.Code)
		}
	})

	t.Run("不存在 404", func(t *testing.T) {
		f := newCancelFixture(t)
		h := NewHandler(f.svc)
		c, w := newReq("")
		h.cancel(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("非法 id 400", func(t *testing.T) {
		f := newCancelFixture(t)
		h := NewHandler(f.svc)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "abc"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/tasks/abc/cancel", nil)
		h.cancel(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
}

// ---------- day41 评审 I-1/I-2：并发复核（CAS 不中 / 置标志窗口被抢先） ----------

func TestCancelPendingRacedByClaimRecovers(t *testing.T) {
	// PENDING CAS 瞬间被 claim 抢先（DISPATCHED）：不返回假 CANCELLED——重读后走标志路径
	f := newCancelFixture(t)
	f.task(t, 11, StatePending)
	f.execution(t, 11, "eid-race1")
	f.repo.casHook = func(taskID int64, from, to string) {
		f.repo.casHook = nil // 只抢一次
		for _, tk := range f.repo.tasks {
			if tk.ID == taskID {
				tk.Status = string(StateDispatched)
			}
		}
	}

	status, err := f.svc.Cancel(11)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if status != string(StateDispatched) {
		t.Fatalf("竞态后应按 DISPATCHED 走标志路径, got %q", status)
	}
	if !f.gate.cancelSet["eid-race1"] {
		t.Fatal("标志未写入（应经 DISPATCHED 路径补上）")
	}
}

func TestCancelRunningRacedByResultConflicts(t *testing.T) {
	// RUNNING CAS 瞬间被 result 抢先（COMPLETED）：应 409 而非假 CANCELLING
	f := newCancelFixture(t)
	f.task(t, 12, StateRunning)
	f.repo.casHook = func(taskID int64, from, to string) {
		f.repo.casHook = nil
		for _, tk := range f.repo.tasks {
			if tk.ID == taskID {
				tk.Status = string(StateCompleted)
			}
		}
	}

	if _, err := f.svc.Cancel(12); !errors.Is(err, ErrCancelConflict) {
		t.Fatalf("被 result 抢先后应 409, got %v", err)
	}
}

func TestCancelDispatchedRacedByFirstStatusRecovers(t *testing.T) {
	// DISPATCHED 置标志窗口内首条 STATUS 已转 RUNNING（评审 I-2）：标志将无人读——复查补转 CANCELLING
	f := newCancelFixture(t)
	f.task(t, 13, StateDispatched)
	f.execution(t, 13, "eid-race2")
	f.repo.findTaskHook = func() {
		f.repo.findTaskHook = nil
		for _, tk := range f.repo.tasks {
			if tk.ID == 13 {
				tk.Status = string(StateRunning) // 模拟 events.go 抢先转移
			}
		}
	}

	status, err := f.svc.Cancel(13)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if status != string(StateCancelling) {
		t.Fatalf("复查后应补转 CANCELLING, got %q", status)
	}
	if got := f.repo.tasks[0].Status; got != string(StateCancelling) {
		t.Fatalf("任务态 = %q, want CANCELLING", got)
	}
}

func TestCancelDispatchedFlagWriteFailureIs500(t *testing.T) {
	// M-7：置标志失败上抛（不静默假成功）
	f := newCancelFixture(t)
	f.task(t, 14, StateDispatched)
	f.execution(t, 14, "eid-race3")
	f.gate.failErr = errors.New("redis down")

	if _, err := f.svc.Cancel(14); err == nil {
		t.Fatal("置标志失败应上抛 500")
	}
}
