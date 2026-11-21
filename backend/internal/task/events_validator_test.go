package task

import (
	"os"
	"testing"
)

const (
	docsEventsSchemaPath = "../../../docs/protocol/schema/events.schema.json"
	eventsSampleDir      = "../../../docs/protocol/schema/samples/"
)

func newTestEventsValidator(t *testing.T) *EventsValidator {
	t.Helper()
	v, err := NewEventsValidator()
	if err != nil {
		t.Fatalf("NewEventsValidator: %v", err)
	}
	return v
}

// TestEventsValidatorAcceptsSamples 三型样例各过（协议冻结件正样本）。
func TestEventsValidatorAcceptsSamples(t *testing.T) {
	v := newTestEventsValidator(t)
	for _, f := range []string{"events.sample.json", "events_log.sample.json", "events_progress.sample.json"} {
		raw, err := os.ReadFile(eventsSampleDir + f)
		if err != nil {
			t.Fatalf("读样例 %s: %v", f, err)
		}
		if err := v.Validate(raw); err != nil {
			t.Errorf("样例 %s 应通过: %v", f, err)
		}
	}
}

// TestEventsValidatorRejects 负样本：三组 type↔payload 错配（day28 Python 侧 I-1 教训的 Go 对称）
// + 结构负例。
func TestEventsValidatorRejects(t *testing.T) {
	v := newTestEventsValidator(t)
	cases := map[string]string{
		"STATUS+message 错配":   `{"seq":0,"ts":"2026-11-19T10:00:00+00:00","type":"STATUS","payload":{"message":"x"}}`,
		"LOG+substate 错配":     `{"seq":1,"ts":"2026-11-19T10:00:00+00:00","type":"LOG","payload":{"substate":"DONE"}}`,
		"PROGRESS 缺 total":    `{"seq":2,"ts":"2026-11-19T10:00:00+00:00","type":"PROGRESS","payload":{"current":1}}`,
		"PROGRESS+message 错配": `{"seq":2,"ts":"2026-11-19T10:00:00+00:00","type":"PROGRESS","payload":{"message":"x","current":1,"total":2}}`,
		"substate 枚举外":        `{"seq":3,"ts":"2026-11-19T10:00:00+00:00","type":"STATUS","payload":{"substate":"BOGUS"}}`,
		"seq 负数":              `{"seq":-1,"ts":"2026-11-19T10:00:00+00:00","type":"LOG","payload":{"message":"x"}}`,
		"缺 ts":                `{"seq":4,"type":"LOG","payload":{"message":"x"}}`,
		"顶层未知键":               `{"seq":5,"ts":"2026-11-19T10:00:00+00:00","type":"LOG","payload":{"message":"x"},"extra":1}`,
		"type 枚举外":            `{"seq":6,"ts":"2026-11-19T10:00:00+00:00","type":"WARN","payload":{"message":"x"}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := v.Validate([]byte(doc)); err == nil {
				t.Fatal("应拒收，实际通过")
			}
		})
	}
}

// TestEventsSchemaCopyMatchesDocs 防漂移钉子（day32 模式）。
func TestEventsSchemaCopyMatchesDocs(t *testing.T) {
	embedRaw, err := schemaFS.ReadFile("schema/events.schema.json")
	if err != nil {
		t.Fatalf("读 embed 副本: %v", err)
	}
	docsRaw, err := os.ReadFile(docsEventsSchemaPath)
	if err != nil {
		t.Fatalf("读 docs 权威件: %v", err)
	}
	if string(embedRaw) != string(docsRaw) {
		t.Fatal("embed 副本与 docs/protocol/schema/events.schema.json 不一致——请同步副本")
	}
}

// TestValidTimestamp ts 后置校验（v6 format no-op 的 Go 原生等价，day34 裁决）：
// RFC3339 带时区过；无时区/垃圾串红。
func TestValidTimestamp(t *testing.T) {
	if !ValidTimestamp("2026-11-19T10:00:00+00:00") {
		t.Error("RFC3339 带时区应通过")
	}
	if !ValidTimestamp("2026-11-19T10:00:00Z") {
		t.Error("Z 后缀应通过")
	}
	if ValidTimestamp("2026-11-19T10:00:00") {
		t.Error("无时区应拒绝（协议：UTC ISO 8601 带时区后缀）")
	}
	if ValidTimestamp("not-a-time") {
		t.Error("垃圾串应拒绝")
	}
}
