package task

import (
	"os"
	"testing"
)

const (
	docsResultSchemaPath = "../../../docs/protocol/schema/result.schema.json"
	resultSamplePath     = "../../../docs/protocol/schema/samples/result.sample.json"
	// v1.1 样例：失败原因 / 故障时间线
	resultFailedSamplePath = "../../../docs/protocol/schema/samples/result_failed.sample.json"
	resultFaultSamplePath  = "../../../docs/protocol/schema/samples/result_fault.sample.json"
)

// validFault v1.1 最小合法故障记录：consumer_kill 已注入、已解除、验证通过（recovered + ok）。
// 八个键全部显式给出（不适用者为 null），与 summary"必填键 + null 表示不适用"的口径一致。
func validFault() map[string]any {
	return map[string]any{
		"strategy": "consumer_kill", "target": "brp_42_consumer",
		"injected_at": "2026-11-24T10:00:10.500+00:00", "recovered_at": "2026-11-24T10:00:25.100+00:00",
		"verified_at": "2026-11-24T10:00:33.000+00:00",
		"status":      "recovered", "recovery_status": "ok", "error": nil,
	}
}

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
	t.Run("v1.1 样例通过（失败原因 / 故障时间线）", func(t *testing.T) {
		for _, p := range []string{resultFailedSamplePath, resultFaultSamplePath} {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读样例 %s: %v", p, err)
			}
			if err := v.Validate(raw); err != nil {
				t.Errorf("样例 %s 应通过: %v", p, err)
			}
		}
	})
	t.Run("v1.1 正例基线（负例表的前提：这几种形态本身必须通过）", func(t *testing.T) {
		m := validResult()
		m["faults"] = []any{validFault()}
		if err := v.Validate(mustJSON(t, m)); err != nil {
			t.Fatalf("validFault 应通过: %v", err)
		}
		stuck := validFault() // 解除失败、故障仍在生效——合法形态
		stuck["status"], stuck["recovered_at"], stuck["verified_at"], stuck["recovery_status"] = "injected", nil, nil, "recovery_failed"
		m = validResult()
		m["faults"] = []any{stuck}
		if err := v.Validate(mustJSON(t, m)); err != nil {
			t.Fatalf("injected+recovery_failed 应通过: %v", err)
		}
	})
	t.Run("负样本表驱动", func(t *testing.T) {
		withFault := func(mutate func(f map[string]any)) func(m map[string]any) {
			return func(m map[string]any) {
				f := validFault()
				mutate(f)
				m["faults"] = []any{f}
			}
		}
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
			// ---- 协议 v1.1：failure / faults ----
			"v1.1 failure 搭配 COMPLETED": func(m map[string]any) {
				m["failure"] = map[string]any{"reason": "timeout", "message": "x"}
			},
			"v1.1 failure.reason 枚举外": func(m map[string]any) {
				m["status"] = "FAILED"
				m["failure"] = map[string]any{"reason": "crashed", "message": "x"}
			},
			"v1.1 failure 缺 message": func(m map[string]any) {
				m["status"] = "FAILED"
				m["failure"] = map[string]any{"reason": "timeout"}
			},
			"v1.1 failure 未知键": func(m map[string]any) {
				m["status"] = "FAILED"
				m["failure"] = map[string]any{"reason": "timeout", "message": "x", "stack": "…"}
			},
			"v1.1 faults 非数组":       func(m map[string]any) { m["faults"] = validFault() },
			"v1.1 faults 项缺 target": withFault(func(f map[string]any) { delete(f, "target") }),
			"v1.1 faults 项缺 recovery_status（须显式 null）": withFault(func(f map[string]any) {
				delete(f, "recovery_status")
			}),
			"v1.1 faults.status 枚举外":          withFault(func(f map[string]any) { f["status"] = "planned" }),
			"v1.1 faults.recovery_status 枚举外": withFault(func(f map[string]any) { f["recovery_status"] = "maybe" }),
			"v1.1 faults 项未知键":                withFault(func(f map[string]any) { f["extra"] = 1 }),
			"v1.1 faults 时间戳类型错": withFault(func(f map[string]any) {
				f["injected_at"] = 1763978410
			}),
			"v1.1 recovered 但 recovered_at 为 null": withFault(func(f map[string]any) { f["recovered_at"] = nil }),
			"v1.1 injected 但 recovered_at 非空":      withFault(func(f map[string]any) { f["status"] = "injected" }),
			"v1.1 failed 但 injected_at 非空":         withFault(func(f map[string]any) { f["status"] = "failed" }),
			// ---- v1.1 规则补全（2026-11-24 评审 I-2：下列自相矛盾的形态曾全部放行）----
			"v1.1 failed 但 recovered_at 有值": withFault(func(f map[string]any) {
				f["status"], f["injected_at"], f["recovery_status"] = "failed", nil, nil
			}),
			"v1.1 failed 但 recovery_status=ok": withFault(func(f map[string]any) {
				f["status"], f["injected_at"], f["recovered_at"], f["verified_at"] = "failed", nil, nil, nil
			}),
			"v1.1 injected 但 recovery_status=ok": withFault(func(f map[string]any) {
				f["status"], f["recovered_at"], f["verified_at"] = "injected", nil, nil
			}),
			"v1.1 injected 但 verified_at 有值": withFault(func(f map[string]any) {
				f["status"], f["recovered_at"], f["recovery_status"] = "injected", nil, nil
			}),
			"v1.1 recovery_status=ok 但 verified_at 为 null": withFault(func(f map[string]any) { f["verified_at"] = nil }),
			"v1.1 faults 超过条数上限": func(m map[string]any) {
				items := make([]any, 0, 33)
				for i := 0; i < 33; i++ {
					items = append(items, validFault())
				}
				m["faults"] = items
			},
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
