package task

import (
	"os"
	"testing"
)

const (
	docsResultSchemaPath = "../../../docs/protocol/schema/result.schema.json"
	resultSamplePath     = "../../../docs/protocol/schema/samples/result.sample.json"
)

func newTestResultValidator(t *testing.T) *ResultValidator {
	t.Helper()
	v, err := NewResultValidator()
	if err != nil {
		t.Fatalf("NewResultValidator: %v", err)
	}
	return v
}

// validResult 最小合法 result 基底（六计数+五指标+verdict+空 details+四差集+total_events）。
func validResult() map[string]any {
	summary := map[string]any{
		"sent": 10, "confirmed": 10, "delivered": 10, "processed_unique": 10, "acked": 10, "dup_business": 0,
		"processed_rate": 1.0, "dup_rate": 0.0, "loss_rate": 0.0,
		"avg_delivery_latency_s": 3.2, "recovery_time_s": nil,
		"verdict": "PASS", "details": []any{},
	}
	recon := map[string]any{
		"dup_business_ids": []any{}, "sent_not_confirmed": []any{}, "confirmed_not_delivered": []any{},
		"delivered_not_processed": []any{}, "processed_not_acked": []any{}, "total_events": 50,
	}
	return map[string]any{
		"attempt": 1, "status": "COMPLETED",
		"summary": summary, "reconciliation": recon, "artifacts": []any{"a.zip"},
	}
}

func TestResultValidator(t *testing.T) {
	v := newTestResultValidator(t)

	t.Run("docs 正样本通过", func(t *testing.T) {
		raw, err := os.ReadFile(resultSamplePath)
		if err != nil {
			t.Fatalf("读样例: %v", err)
		}
		if err := v.Validate(raw); err != nil {
			t.Fatalf("样例应通过: %v", err)
		}
	})
	t.Run("五指标 null 均合法", func(t *testing.T) {
		m := validResult()
		sum := m["summary"].(map[string]any)
		for _, k := range []string{"processed_rate", "dup_rate", "loss_rate", "avg_delivery_latency_s", "recovery_time_s"} {
			sum[k] = nil
		}
		if err := v.Validate(mustJSON(t, m)); err != nil {
			t.Fatalf("全 null 指标应通过（场景无故障数据）: %v", err)
		}
	})
	t.Run("负样本表驱动", func(t *testing.T) {
		cases := map[string]func(m map[string]any){
			"status 枚举外":  func(m map[string]any) { m["status"] = "RUNNING" },
			"verdict 枚举外": func(m map[string]any) { m["summary"].(map[string]any)["verdict"] = "MAYBE" },
			"缺六计数之一":      func(m map[string]any) { delete(m["summary"].(map[string]any), "acked") },
			"率越界 1.5":     func(m map[string]any) { m["summary"].(map[string]any)["loss_rate"] = 1.5 },
			"details 项缺 actual": func(m map[string]any) {
				m["summary"].(map[string]any)["details"] = []any{map[string]any{"metric": "dup_rate", "op": "<=", "value": 0, "passed": true}}
			},
			"attempt 为 0": func(m map[string]any) { m["attempt"] = 0 },
			"顶层未知键":       func(m map[string]any) { m["extra"] = 1 },
			"reconciliation 缺 total_events": func(m map[string]any) {
				delete(m["reconciliation"].(map[string]any), "total_events")
			},
			"artifacts 非 string 项": func(m map[string]any) { m["artifacts"] = []any{1} },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				m := validResult()
				mutate(m)
				if err := v.Validate(mustJSON(t, m)); err == nil {
					t.Fatal("应拒收，实际通过")
				}
			})
		}
	})
	t.Run("防漂移钉子", func(t *testing.T) {
		embedRaw, err := schemaFS.ReadFile("schema/result.schema.json")
		if err != nil {
			t.Fatalf("读 embed 副本: %v", err)
		}
		docsRaw, err := os.ReadFile(docsResultSchemaPath)
		if err != nil {
			t.Fatalf("读 docs 权威件: %v", err)
		}
		if string(embedRaw) != string(docsRaw) {
			t.Fatal("embed 副本与 docs/protocol/schema/result.schema.json 不一致——请同步副本")
		}
	})
}
