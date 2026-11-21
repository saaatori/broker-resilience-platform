// events_validator.go 事件请求体校验（M2 第 34 天）：
// 权威 = docs/protocol/schema/events.schema.json（v1.0.3 顶层 oneOf 三型，type const 绑定 payload），
// embed 副本 + 防漂移钉子（events_validator_test）。ts 的 date-time 用 Go 原生 RFC3339 后置校验——
// v6 对 format 默认 no-op（day28 记档教训），注册 format 回调繁琐且等价，裁决记档。
package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// eventsSchemaResourceName embed 资源名与 compiler URL。
const eventsSchemaResourceName = "events.schema.json"

// EventsValidator 事件请求体 JSON Schema 校验器（编译一次复用）。
type EventsValidator struct {
	sch *jsonschema.Schema
}

// NewEventsValidator 从 embed 副本编译（AddResource 收解码值——v6 坑，见 validator.go 注释）。
func NewEventsValidator() (*EventsValidator, error) {
	raw, err := schemaFS.ReadFile("schema/" + eventsSchemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("read embedded events schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode embedded events schema: %w", err)
	}
	if err := c.AddResource(eventsSchemaResourceName, schemaValue); err != nil {
		return nil, fmt.Errorf("load events schema resource: %w", err)
	}
	sch, err := c.Compile(eventsSchemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("compile events schema: %w", err)
	}
	return &EventsValidator{sch: sch}, nil
}

// Validate 校验事件请求体字节（oneOf 三型直接整校——错配即拒收）。统一 UseNumber 解码。
func (v *EventsValidator) Validate(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return fmt.Errorf("事件不是合法 JSON: %w", err)
	}
	if err := v.sch.Validate(val); err != nil {
		return fmt.Errorf("事件不符合 schema（type↔payload 三型）: %w", err)
	}
	return nil
}

// ValidTimestamp ts 后置校验：RFC3339（必然带时区偏移或 Z——协议 §2.4 v1.0.1）。
func ValidTimestamp(ts string) bool {
	_, err := time.Parse(time.RFC3339, ts)
	return err == nil
}
