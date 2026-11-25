// result_validator.go 结果回传体校验（M2 第 35 天）：
// 权威 = docs/protocol/schema/result.schema.json（v1.0.2 键集=v1.0.3 收紧——六计数+五指标(null 可)
// +verdict/details+四差集+total_events；v1.1 增 failure / faults），embed 副本+防漂移钉子。
// 三件 schema 的最后一件 Go 侧落地。faults 时间戳的 RFC3339 不在 schema 层生效（v6 format no-op），
// 由 result.go 的 checkFaults 后置校验。
package task

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// resultSchemaResourceName embed 资源名与 compiler URL。
const resultSchemaResourceName = "result.schema.json"

// ResultValidator 结果回传体校验器（编译一次复用）。
type ResultValidator struct {
	sch *jsonschema.Schema
}

// NewResultValidator 从 embed 副本编译（AddResource 收解码值——v6 坑见 validator.go）。
func NewResultValidator() (*ResultValidator, error) {
	raw, err := schemaFS.ReadFile("schema/" + resultSchemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("read embedded result schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode embedded result schema: %w", err)
	}
	if err := c.AddResource(resultSchemaResourceName, schemaValue); err != nil {
		return nil, fmt.Errorf("load result schema resource: %w", err)
	}
	sch, err := c.Compile(resultSchemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("compile result schema: %w", err)
	}
	return &ResultValidator{sch: sch}, nil
}

// Validate 校验结果回传体字节。统一 UseNumber 解码。
func (v *ResultValidator) Validate(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return fmt.Errorf("结果不是合法 JSON: %w", err)
	}
	if err := v.sch.Validate(val); err != nil {
		return fmt.Errorf("结果不符合 schema（summary/reconciliation/failure/faults）: %w", err)
	}
	return nil
}
