package model

import "time"

// TestResult 执行结果表（总体设计表 22）：与执行 1:1（uq(execution_id)=结果回传幂等 first-wins）。
// metrics 存 summary 的指标全量（五指标+verdict+details），reconciliation 存四差集证据原文；
// 结构化查询走 test_metric_summary 关系列（表 25 物理设计约定）。
type TestResult struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	ExecutionID    string    `gorm:"type:uuid;not null;uniqueIndex:uq_result_execution" json:"execution_id"` // 表 25：uq(execution_id)
	Verdict        string    `gorm:"size:8;not null" json:"verdict"`                                         // PASS / FAIL
	Metrics        string    `gorm:"type:jsonb;not null" json:"-"`
	Reconciliation string    `gorm:"type:jsonb;not null" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestResult) TableName() string { return "test_result" }
