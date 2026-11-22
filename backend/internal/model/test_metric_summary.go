package model

import "time"

// TestMetricSummary 指标汇总表（总体设计表 23）：五指标展开为可查询关系列（支撑报告对比）。
// null 指标不落行（指标无值=场景不适用，如正常场景的 recovery_time_s）。
type TestMetricSummary struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	ExecutionID string    `gorm:"type:uuid;not null;uniqueIndex:uq_summary_exec_metric,priority:1" json:"execution_id"` // 表 25：uq(execution_id, metric_key)——前缀覆盖单列查询，无需独立 idx
	MetricKey   string    `gorm:"size:64;not null;uniqueIndex:uq_summary_exec_metric,priority:2" json:"metric_key"`
	Value       float64   `gorm:"type:numeric;not null" json:"value"`
	Unit        string    `gorm:"size:16;not null" json:"unit"` // % / s（表 23 单位枚举内的语义子集）
	CreatedAt   time.Time `json:"created_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestMetricSummary) TableName() string { return "test_metric_summary" }
