package task

import (
	"errors"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// newResultFixture result 造数：agent(id=7) + RUNNING task(id=42) + CLAIMED exec(eid-1)
// + 真 ResultValidator + fake 闸门 + Hub 订阅。
func newResultFixture(t *testing.T, taskStatus string) (*Service, *fakeTaskRepo, *fakeEventGate, *model.AgentInstance, <-chan SSEEvent) {
	t.Helper()
	repo := newFakeTaskRepo()
	gate := &fakeEventGate{seen: map[string]bool{}, cancelSet: map[string]bool{}, removedCancel: map[string]bool{}}
	hub := NewHub()
	ch, cancel := hub.Subscribe(42)
	t.Cleanup(cancel)
	repo.tasks = append(repo.tasks, &model.TestTask{ID: 42, ScenarioID: 1, Status: taskStatus})
	repo.execRows = []model.TestExecution{{ID: 1, ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Attempt: 1, Status: "CLAIMED"}}
	agent := &model.AgentInstance{ID: 7, AgentID: "a-uuid", Status: "online"}
	svc := NewService(repo, ServiceDeps{Result: newTestResultValidator(t), Gate: gate, Hub: hub})
	return svc, repo, gate, agent, ch
}

func resultJSON(status string, attempt int) []byte {
	return []byte(`{"attempt":` + itoa(attempt) + `,"status":"` + status + `","summary":{"sent":10,"confirmed":10,"delivered":10,"processed_unique":10,"acked":10,"dup_business":0,"processed_rate":1.0,"dup_rate":0.0,"loss_rate":0.0,"avg_delivery_latency_s":3.2,"recovery_time_s":null,"verdict":"PASS","details":[]},"reconciliation":{"dup_business_ids":[],"sent_not_confirmed":[],"confirmed_not_delivered":[],"delivered_not_processed":[],"processed_not_acked":[],"total_events":50},"artifacts":["e1.zip","e2.zip"]}`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestDecideTransition 15 格决策表（协议 §4 转移表含 ※※/I-1/I-4 的全积）。
func TestDecideTransition(t *testing.T) {
	cases := []struct {
		task, result, want string // want=""=不改态
	}{
		{"DISPATCHED", "COMPLETED", "COMPLETED"}, // v1.0.4 ※※（STATUS 前崩溃恢复直传）
		{"DISPATCHED", "FAILED", "FAILED"},       // v1.0.4 ※※
		{"DISPATCHED", "CANCELLED", ""},          // v1.0.5 I-1：照录不改态（清扫收口）
		{"RUNNING", "COMPLETED", "COMPLETED"},
		{"RUNNING", "FAILED", "FAILED"},
		{"RUNNING", "CANCELLED", ""},             // v1.0.5 I-1
		{"CANCELLING", "CANCELLED", "CANCELLED"}, // v1.0.1 ※：视同 cleanup-ack
		{"CANCELLING", "COMPLETED", "CANCELLED"}, // v1.0.4 表行 9：取消优先，任务转取消终态（result 照录）
		{"CANCELLING", "FAILED", "CANCELLED"},    // v1.0.4 表行 9
		{"COMPLETED", "COMPLETED", ""},           // 终态兜底：不改
		{"COMPLETED", "FAILED", ""},
		{"COMPLETED", "CANCELLED", ""}, // 终态兜底 9 格全覆盖
		{"FAILED", "COMPLETED", ""},
		{"CANCELLED", "COMPLETED", ""},
		{"PENDING", "COMPLETED", ""}, // 防御格：PENDING 不可能有执行回传
	}
	for _, tc := range cases {
		t.Run(tc.task+"×"+tc.result, func(t *testing.T) {
			got := decideTransition(tc.task, tc.result)
			if got != tc.want {
				t.Fatalf("task=%s result=%s: want %q got %q", tc.task, tc.result, tc.want, got)
			}
		})
	}
}

func TestResult(t *testing.T) {
	t.Run("归属两路 404", func(t *testing.T) {
		svc, _, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "no-such", resultJSON("COMPLETED", 1)); !errors.Is(err, ErrExecutionNotFound) {
			t.Fatalf("want 404 哨兵, got %v", err)
		}
		other := &model.AgentInstance{ID: 999}
		if err := svc.Result(other, "eid-1", resultJSON("COMPLETED", 1)); !errors.Is(err, ErrExecutionNotFound) {
			t.Fatalf("他人 eid 应同一 404, got %v", err)
		}
	})
	t.Run("attempt 不匹配→400", func(t *testing.T) {
		svc, _, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 2)); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("want ErrInvalidResult, got %v", err)
		}
	})
	t.Run("schema 负例→400", func(t *testing.T) {
		svc, _, _, agent, _ := newResultFixture(t, "RUNNING")
		bad := []byte(`{"attempt":1,"status":"COMPLETED"}`) // 缺 summary/reconciliation/artifacts
		if err := svc.Result(agent, "eid-1", bad); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("want ErrInvalidResult, got %v", err)
		}
	})
	t.Run("RUNNING+COMPLETED 全链", func(t *testing.T) {
		svc, repo, gate, agent, ch := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "COMPLETED" {
			t.Errorf("task 应 COMPLETED: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != "DONE" {
			t.Errorf("exec 子状态应 DONE: %s", repo.execRows[0].Status)
		}
		if len(repo.resultRows) != 1 || repo.resultRows["eid-1"].Verdict != "PASS" {
			t.Errorf("result 行应 1 条 verdict=PASS: %+v", repo.resultRows)
		}
		if len(repo.summaryRows) != 4 { // 五指标 recovery_time_s=null 跳过
			t.Errorf("summary 应 4 行（null 跳过），实际 %d: %+v", len(repo.summaryRows), repo.summaryRows)
		}
		for _, row := range repo.summaryRows {
			if want := metricUnits[row.MetricKey]; row.Unit != want {
				t.Errorf("指标 %s 单位应 %s，实际 %s", row.MetricKey, want, row.Unit)
			}
		}
		if len(repo.reportRows) != 2 {
			t.Errorf("artifacts 应 2 行，实际 %d", len(repo.reportRows))
		}
		if !gate.removedCancel["eid-1"] {
			t.Error("exec:cancel 键应删除")
		}
		if len(ch) == 0 {
			t.Error("终态应广播 task 事件")
		}
	})
	t.Run("RUNNING+FAILED→FAILED+DONE", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSON("FAILED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "FAILED" || repo.execRows[0].Status != "DONE" {
			t.Errorf("FAILED/DONE 不符: %s/%s", repo.tasks[0].Status, repo.execRows[0].Status)
		}
	})
	t.Run("CANCELLING+CANCELLED→CANCELLED+ABORTED", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "CANCELLING")
		if err := svc.Result(agent, "eid-1", resultJSON("CANCELLED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "CANCELLED" || repo.execRows[0].Status != "ABORTED" {
			t.Errorf("CANCELLED/ABORTED 不符: %s/%s", repo.tasks[0].Status, repo.execRows[0].Status)
		}
	})
	t.Run("CANCELLING+COMPLETED：取消优先，result 照录不改态", func(t *testing.T) {
		svc, repo, _, agent, ch := newResultFixture(t, "CANCELLING")
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "CANCELLED" {
			t.Fatalf("取消优先应转取消终态 CANCELLED（表行 9）: %s", repo.tasks[0].Status)
		}
		if len(repo.resultRows) != 1 {
			t.Fatal("result 应照录（保留原 status）")
		}
		if len(repo.reportRows) != 2 || len(repo.summaryRows) != 4 {
			t.Fatal("三表登记应照常（v1.0.5 I-4）")
		}
		if len(ch) == 0 {
			t.Error("转移到 CANCELLED 应广播 task 事件")
		}
	})
	t.Run("RUNNING+CANCELLED：照录不改态+exec ABORTED", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSON("CANCELLED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "RUNNING" {
			t.Fatalf("非取消态 CANCELLED 照录不改态: %s", repo.tasks[0].Status)
		}
		if repo.execRows[0].Status != "ABORTED" {
			t.Error("执行侧终了应 ABORTED")
		}
		if len(repo.resultRows) != 1 {
			t.Error("result 应照录")
		}
	})
	t.Run("first-wins：重复与尾随全部 200 零副作用", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "CANCELLING")
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 1)); err != nil {
			t.Fatalf("首次: %v", err)
		}
		// 尾随 result(CANCELLED)（v1.0.5 I-4 明文场景）——first-wins 忽略
		if err := svc.Result(agent, "eid-1", resultJSON("CANCELLED", 1)); err != nil {
			t.Fatalf("尾随应 nil: %v", err)
		}
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 1)); err != nil {
			t.Fatalf("重复应 nil: %v", err)
		}
		if len(repo.resultRows) != 1 || len(repo.reportRows) != 2 || len(repo.summaryRows) != 4 {
			t.Fatal("重复/尾随不得产生任何新行")
		}
		if repo.tasks[0].Status != "CANCELLED" {
			t.Fatalf("首落按表行 9 应转 CANCELLED，尾随不得再改: %s", repo.tasks[0].Status)
		}
	})
	t.Run("cancel 键删除失败不阻断", func(t *testing.T) {
		svc, repo, gate, agent, _ := newResultFixture(t, "RUNNING")
		gate.removeFail = true
		if err := svc.Result(agent, "eid-1", resultJSON("COMPLETED", 1)); err != nil {
			t.Fatalf("键删除失败不应阻断: %v", err)
		}
		if repo.tasks[0].Status != "COMPLETED" {
			t.Fatal("主链路应完成")
		}
	})
}

// TestSummaryMetricsStoredScale 量纲钉子（代码走查 #2）：率类指标原始值域 [0,1]（metrics.py 契约），
// 表 23 单位为 %——入库须换算为百分数值，使 value 与 unit 自洽（1.0 存成 100 而不是 1.0）。
// 时长类原值入库；null 指标不落行。
func TestSummaryMetricsStoredScale(t *testing.T) {
	summary := []byte(`{"sent":10000,"processed_rate":1.0,"dup_rate":0.0,"loss_rate":0.9999,` +
		`"avg_delivery_latency_s":3.2,"recovery_time_s":null,"verdict":"PASS","details":[]}`)
	got := summaryMetrics(summary)
	want := map[string]float64{
		"processed_rate": 100, "dup_rate": 0, "loss_rate": 99.99, "avg_delivery_latency_s": 3.2,
	}
	if len(got) != len(want) {
		t.Fatalf("应落 %d 个指标（recovery_time_s=null 跳过），实际 %d: %v", len(want), len(got), got)
	}
	for key, w := range want {
		if diff := got[key] - w; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: want %v got %v", key, w, got[key])
		}
	}
	for _, rate := range []string{"processed_rate", "dup_rate", "loss_rate"} {
		if metricUnits[rate] != "%" {
			t.Errorf("率类 %s 单位应为 %%", rate)
		}
	}
}
