package task

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// docs 权威路径（防漂移钉子与正样本取自协议冻结件，副本不自我证明）。
const (
	docsSchemaPath = "../../../docs/protocol/schema/params_snapshot.schema.json"
	samplePath     = "../../../docs/protocol/schema/samples/params.sample.json"
)

func newTestValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// validSnapshot 最小合法快照基底：四 required 键 + 一条合法断言。
func validSnapshot() map[string]any {
	return map[string]any{
		"message_count":  100,
		"publish_rate":   50,
		"idle_timeout_s": 30,
		"assertions": []any{map[string]any{
			"metric": "processed_rate", "op": ">=", "value": 1.0,
		}},
	}
}

// TestValidatorAcceptsSample docs 正样本通过校验（计划 Task 2 验收原文）。
func TestValidatorAcceptsSample(t *testing.T) {
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("读样例: %v", err)
	}
	if err := newTestValidator(t).Validate(raw); err != nil {
		t.Fatalf("样例应通过校验: %v", err)
	}
}

// TestValidatorAcceptsEmptyAssertions 空 assertions 合法（schema 无 minItems；
// day25 裁决：显式空列表=vacuous PASS，是场景层合法语义）。
func TestValidatorAcceptsEmptyAssertions(t *testing.T) {
	m := validSnapshot()
	m["assertions"] = []any{}
	if err := newTestValidator(t).Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("空 assertions 应通过: %v", err)
	}
}

// TestValidatorRejectsInvalid 负样本表驱动：七类违规各拒绝。
func TestValidatorRejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"metric 枚举外", func(m map[string]any) {
			m["assertions"].([]any)[0].(map[string]any)["metric"] = "bogus_rate"
		}},
		{"op 枚举外", func(m map[string]any) {
			m["assertions"].([]any)[0].(map[string]any)["op"] = "!="
		}},
		{"message_count 为 0", func(m map[string]any) { m["message_count"] = 0 }},
		{"缺 publish_rate", func(m map[string]any) { delete(m, "publish_rate") }},
		{"顶层未知键", func(m map[string]any) { m["extra"] = 1 }},
		{"fault 缺 duration_s", func(m map[string]any) {
			m["fault"] = map[string]any{"strategy": "consumer_kill", "inject_at_s": 5}
		}},
		{"断言项缺 value", func(m map[string]any) {
			m["assertions"] = []any{map[string]any{"metric": "dup_rate", "op": "<="}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validSnapshot()
			tc.mutate(m)
			if err := newTestValidator(t).Validate(mustJSON(t, m)); err == nil {
				t.Fatal("应拒绝该快照，实际通过")
			}
		})
	}
}

// TestSchemaCopyMatchesDocs 防漂移钉子：embed 副本与 docs 权威件逐字节相等——
// 协议 schema 升版本而副本未跟进时此处变红（day28 round-trip 钉子的 Go 侧对应物）。
func TestSchemaCopyMatchesDocs(t *testing.T) {
	embedRaw, err := schemaFS.ReadFile("schema/params_snapshot.schema.json")
	if err != nil {
		t.Fatalf("读 embed 副本: %v", err)
	}
	docsRaw, err := os.ReadFile(docsSchemaPath)
	if err != nil {
		t.Fatalf("读 docs 权威件: %v", err)
	}
	if !bytes.Equal(embedRaw, docsRaw) {
		t.Fatal("embed 副本与 docs/protocol/schema/params_snapshot.schema.json 不一致——请同步副本")
	}
}
