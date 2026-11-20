// Package task 任务创建（M2 第 31 天）：params_snapshot 校验、场景 seed、合成与落库。
// schema 权威 = docs/protocol/schema/params_snapshot.schema.json（协议 v1.0.5 冻结件），
// 此处 embed 副本仅为构建产物自包含——防漂移钉子（validator_test）保证两者逐字节一致。
package task

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema/params_snapshot.schema.json
var schemaFS embed.FS

// schemaResourceName embed 内的资源名与 compiler 资源 URL（同一字符串，AddResource 与 Compile 对应）。
const schemaResourceName = "params_snapshot.schema.json"

// Validator params_snapshot JSON Schema 校验器；编译一次进程内复用。
type Validator struct {
	sch *jsonschema.Schema
}

// NewValidator 从 embed 副本编译 schema。副本为协议冻结件，内容恒合法——
// 失败只可能来自库升级或 embed 破损，调用方（main）按 fail-fast 处理。
func NewValidator() (*Validator, error) {
	raw, err := schemaFS.ReadFile("schema/" + schemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	// v6 的 AddResource 收解码后的 JSON 值（不自动读 Reader——传 Reader 会被当作值本身校验）；
	// UnmarshalJSON 保留 json.Number，与 Validate 侧的 UseNumber 口径一致。
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode embedded schema: %w", err)
	}
	if err := c.AddResource(schemaResourceName, schemaValue); err != nil {
		return nil, fmt.Errorf("load schema resource: %w", err)
	}
	sch, err := c.Compile(schemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return &Validator{sch: sch}, nil
}

// Validate 校验合成后的快照 JSON 字节。统一 UseNumber 解码：整数保持 json.Number
// 形态（10000 不经 float64 往返，schema integer 判定与 wire 输出双保险）。
// 通过返回 nil；失败返回带 schema 违规摘要的 error（handler 转 400 文案）。
func (v *Validator) Validate(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return fmt.Errorf("快照不是合法 JSON: %w", err)
	}
	if err := v.sch.Validate(val); err != nil {
		return fmt.Errorf("params_snapshot 不合法: %w", err)
	}
	return nil
}
