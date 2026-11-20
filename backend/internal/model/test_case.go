package model

// TestScenario 所属测试用例表（总体设计表 18）。M2 仅建表（test_task.case_id 外键指向），
// 用例级 CRUD 与 steps/assertions 编辑不在 day31 范围。
// 表 18 无时间列（与总体设计原文一致，非遗漏）。
// Assertions 为 jsonb 字符串（断言 schema 见 docs/protocol/schema/params_snapshot.schema.json）。
type TestCase struct {
	ID         int64   `gorm:"primaryKey" json:"id"`
	ScenarioID int64   `gorm:"not null" json:"scenario_id"`
	Name       string  `gorm:"size:128;not null" json:"name"`
	Steps      *string `gorm:"type:jsonb" json:"steps,omitempty"`
	Assertions string  `gorm:"type:jsonb;not null" json:"-"`
	Enabled    bool    `gorm:"default:true" json:"enabled"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestCase) TableName() string { return "test_case" }
