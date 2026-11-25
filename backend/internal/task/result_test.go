package task

import (
	"bytes"
	"errors"
	"strings"
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
	return resultJSONExtra(status, attempt, "")
}

// resultJSONExtra 在 status 之后插入额外的顶层键（协议 v1.1 的 failure / faults 等），其余同 resultJSON。
func resultJSONExtra(status string, attempt int, extra string) []byte {
	if extra != "" {
		extra += ","
	}
	return []byte(`{"attempt":` + itoa(attempt) + `,"status":"` + status + `",` + extra + `"summary":{"sent":10,"confirmed":10,"delivered":10,"processed_unique":10,"acked":10,"dup_business":0,"processed_rate":1.0,"dup_rate":0.0,"loss_rate":0.0,"avg_delivery_latency_s":3.2,"recovery_time_s":null,"verdict":"PASS","details":[]},"reconciliation":{"dup_business_ids":[],"sent_not_confirmed":[],"confirmed_not_delivered":[],"delivered_not_processed":[],"processed_not_acked":[],"total_events":50},"artifacts":["e1.zip","e2.zip"]}`)
}

// faultJSON v1.1 单条故障记录（八键显式；injectedAt 可替换以构造时间戳负例）：已注入、仍在生效。
func faultJSON(injectedAt string) string {
	return `"faults":[{"strategy":"consumer_kill","target":"brp_42_consumer","injected_at":` + injectedAt +
		`,"recovered_at":null,"verified_at":null,"status":"injected","recovery_status":null,"error":null}]`
}

// faultRecoveredJSON v1.1 单条"已解除且验证通过"的故障记录，三个时间戳由调用方给出（JSON 字面量）。
func faultRecoveredJSON(injectedAt, recoveredAt, verifiedAt string) string {
	return `"faults":[{"strategy":"consumer_kill","target":"brp_42_consumer","injected_at":` + injectedAt +
		`,"recovered_at":` + recoveredAt + `,"verified_at":` + verifiedAt +
		`,"status":"recovered","recovery_status":"ok","error":null}]`
}

// faultTS 测试用时间戳字面量：2026-11-24T10:00:<ss>+00:00。
func faultTS(ss string) string { return `"2026-11-24T10:00:` + ss + `+00:00"` }

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

	// ---- 协议 v1.1：failure（失败原因）/ faults（故障时间线）----
	t.Run("v1.1 FAILED+failure(timeout)：exit_reason=timeout 且 failure 原文落库", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("FAILED", 1, `"failure":{"reason":"timeout","message":"pytest 超过总超时 300s，已 SIGKILL"}`)
		if err := svc.Result(agent, "eid-1", raw); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if repo.tasks[0].Status != "FAILED" {
			t.Fatalf("task 应 FAILED: %s", repo.tasks[0].Status)
		}
		if got := repo.execRows[0].ExitReason; got == nil || *got != "timeout" {
			t.Fatalf("exit_reason 应取 failure.reason=timeout，实际 %v", got)
		}
		r := repo.resultRows["eid-1"]
		if r == nil || r.Failure == nil || !strings.Contains(*r.Failure, `"reason":"timeout"`) {
			t.Fatalf("failure 应原文落 test_result.failure，实际 %+v", r)
		}
	})
	t.Run("v1.1 FAILED+failure(error)：exit_reason=error 且 failure 落库", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("FAILED", 1, `"failure":{"reason":"error","message":"lab up failed: broker unhealthy"}`)
		if err := svc.Result(agent, "eid-1", raw); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if got := repo.execRows[0].ExitReason; got == nil || *got != "error" {
			t.Fatalf("exit_reason 应 error，实际 %v", got)
		}
		// 判别点在落库内容：兜底值与本例的显式 reason 相同，只看 exit_reason 的话"实现完全忽略 failure"也能过
		if r := repo.resultRows["eid-1"]; r == nil || r.Failure == nil || !strings.Contains(*r.Failure, `"reason":"error"`) {
			t.Fatalf("failure 应原文落库，实际 %+v", r)
		}
	})
	t.Run("v1.1 FAILED 未带 failure：exit_reason 兜底 error、failure 列为空（不因缺原因丢结果）", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSON("FAILED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if got := repo.execRows[0].ExitReason; got == nil || *got != "error" {
			t.Fatalf("exit_reason 应兜底 error，实际 %v", got)
		}
		if repo.resultRows["eid-1"].Failure != nil {
			t.Fatalf("无 failure 时列应为空，实际 %s", *repo.resultRows["eid-1"].Failure)
		}
	})
	t.Run("v1.1 COMPLETED+failure：failure 仅限 FAILED，schema 拒收 400", func(t *testing.T) {
		svc, _, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("COMPLETED", 1, `"failure":{"reason":"timeout","message":"x"}`)
		if err := svc.Result(agent, "eid-1", raw); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("want ErrInvalidResult, got %v", err)
		}
	})
	t.Run("v1.1 COMPLETED+faults：接受并照常落结果行", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("COMPLETED", 1, faultJSON(`"2026-11-24T10:00:10.500+00:00"`))
		if err := svc.Result(agent, "eid-1", raw); err != nil {
			t.Fatalf("合法 faults 应接受: %v", err)
		}
		if len(repo.resultRows) != 1 || repo.tasks[0].Status != "COMPLETED" {
			t.Fatalf("应照常完成并落结果行: rows=%d status=%s", len(repo.resultRows), repo.tasks[0].Status)
		}
	})
	t.Run("v1.1 faults 时间戳无时区：schema 的 format 在 Go 侧不生效，须由后置校验拒收", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("COMPLETED", 1, faultJSON(`"2026-11-24T10:00:10"`))
		if err := svc.Result(agent, "eid-1", raw); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("want ErrInvalidResult, got %v", err)
		}
		if len(repo.resultRows) != 0 || repo.tasks[0].Status != "RUNNING" {
			t.Fatalf("被拒收的回传不得落库或改态: rows=%d status=%s", len(repo.resultRows), repo.tasks[0].Status)
		}
	})
	t.Run("v1.1 首次 FAILED 带原因，尾随回传不改写（first-wins）", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.Result(agent, "eid-1", resultJSONExtra("FAILED", 1, `"failure":{"reason":"timeout","message":"t"}`)); err != nil {
			t.Fatalf("首次: %v", err)
		}
		if err := svc.Result(agent, "eid-1", resultJSON("FAILED", 1)); err != nil {
			t.Fatalf("重复应 nil: %v", err)
		}
		if !strings.Contains(*repo.resultRows["eid-1"].Failure, "timeout") {
			t.Fatal("首落的 failure 不得被尾随回传改写")
		}
	})

	// ---- v1.1 规则补全（2026-11-24 评审 I-2 / I-3）----
	rejected := func(t *testing.T, svc *Service, repo *fakeTaskRepo, agent *model.AgentInstance, raw []byte, label string) {
		t.Helper()
		if err := svc.Result(agent, "eid-1", raw); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("%s：want ErrInvalidResult, got %v", label, err)
		}
		if len(repo.resultRows) != 0 || repo.tasks[0].Status != "RUNNING" {
			t.Fatalf("%s：被拒收的回传不得落库或改态: rows=%d status=%s", label, len(repo.resultRows), repo.tasks[0].Status)
		}
	}
	t.Run("v1.1 faults 时间先后颠倒（recovered 早于 injected）→ 400", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		rejected(t, svc, repo, agent, resultJSONExtra("COMPLETED", 1, faultRecoveredJSON(faultTS("25"), faultTS("10"), faultTS("40"))), "recovered 早于 injected")
	})
	t.Run("v1.1 faults 时间先后颠倒（verified 早于 recovered）→ 400", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		rejected(t, svc, repo, agent, resultJSONExtra("COMPLETED", 1, faultRecoveredJSON(faultTS("10"), faultTS("25"), faultTS("20"))), "verified 早于 recovered")
	})
	t.Run("v1.1 faults 时间戳先后合法：接受并落结果行", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("COMPLETED", 1, faultRecoveredJSON(faultTS("10"), faultTS("25"), faultTS("40")))
		if err := svc.Result(agent, "eid-1", raw); err != nil {
			t.Fatalf("合法时间序应接受: %v", err)
		}
		if len(repo.resultRows) != 1 || repo.tasks[0].Status != "COMPLETED" {
			t.Fatalf("应照常完成并落结果行: rows=%d status=%s", len(repo.resultRows), repo.tasks[0].Status)
		}
	})
	t.Run("v1.1 faults recovered_at / verified_at 时间戳无时区：后置校验拒收", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		rejected(t, svc, repo, agent, resultJSONExtra("COMPLETED", 1,
			faultRecoveredJSON(faultTS("10"), `"2026-11-24T10:00:25"`, faultTS("40"))), "recovered_at 无时区")
		svc2, repo2, _, agent2, _ := newResultFixture(t, "RUNNING")
		rejected(t, svc2, repo2, agent2, resultJSONExtra("COMPLETED", 1,
			faultRecoveredJSON(faultTS("10"), faultTS("25"), `"2026-11-24T10:00:40"`)), "verified_at 无时区")
	})
	t.Run("failure.message 含 \\u0000：jsonb 存不了，拒收 400 而不是落库时 500", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		rejected(t, svc, repo, agent, resultJSONExtra("FAILED", 1, `"failure":{"reason":"error","message":"bad\u0000char"}`), "NUL 转义")
	})
	t.Run("回传体含非法 UTF-8 字节：拒收 400", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := bytes.Replace(resultJSONExtra("FAILED", 1, `"failure":{"reason":"error","message":"PLACEHOLDER"}`),
			[]byte("PLACEHOLDER"), []byte{0xff, 0xfe}, 1)
		rejected(t, svc, repo, agent, raw, "非法 UTF-8")
	})
	t.Run("failure.message 含中文等多字节字符：正常接受并落库", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		raw := resultJSONExtra("FAILED", 1, `"failure":{"reason":"error","message":"实验环境拉起失败：broker 未就绪 🙂"}`)
		if err := svc.Result(agent, "eid-1", raw); err != nil {
			t.Fatalf("多字节字符应接受: %v", err)
		}
		if r := repo.resultRows["eid-1"]; r == nil || r.Failure == nil || !strings.Contains(*r.Failure, "broker 未就绪") {
			t.Fatalf("failure 应落库，实际 %+v", r)
		}
	})

	// ---- 终态子状态先到（2026-11-24 评审 I-1）----
	t.Run("终态子状态先到：result 仍收口 exit_reason 与 finished_at", func(t *testing.T) {
		svc, repo, _, agent, _ := newResultFixture(t, "RUNNING")
		if err := svc.repo.UpdateExecSubstate("eid-1", "ABORTED"); err != nil { // STATUS(ABORTED) 的落库效果
			t.Fatalf("substate: %v", err)
		}
		if err := svc.Result(agent, "eid-1", resultJSON("CANCELLED", 1)); err != nil {
			t.Fatalf("Result: %v", err)
		}
		e := repo.execRows[0]
		if e.Status != "ABORTED" || e.ExitReason == nil || *e.ExitReason != "cancelled" || e.FinishedAt == nil {
			t.Fatalf("want ABORTED/cancelled/finished_at, got %s/%v/%v", e.Status, e.ExitReason, e.FinishedAt)
		}
	})
	t.Run("已收口的执行不被后写覆盖（先收口者固化）", func(t *testing.T) {
		repo := newFakeTaskRepo()
		repo.execRows = []model.TestExecution{{ID: 1, ExecutionID: "eid-1", TaskID: 42, AgentID: 7, Attempt: 1, Status: "RUNNING"}}
		if err := repo.FinishExecution("eid-1", "DONE", ""); err != nil {
			t.Fatalf("首次收口: %v", err)
		}
		if err := repo.FinishExecution("eid-1", "ABORTED", "lease_lost"); err != nil {
			t.Fatalf("后写: %v", err)
		}
		if e := repo.execRows[0]; e.Status != "DONE" || e.ExitReason != nil {
			t.Fatalf("已收口的执行不得被覆盖: %s/%v", e.Status, e.ExitReason)
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
