package model

import "time"

// TestResult 执行结果表（总体设计表 22）：与执行 1:1（uq(execution_id)=结果回传幂等 first-wins）。
// metrics 存 summary 的指标全量（五指标+verdict+details），reconciliation 存四差集证据原文；
// 结构化查询走 test_metric_summary 关系列（表 25 物理设计约定）。
type TestResult struct {
	ID             int64  `gorm:"primaryKey" json:"id"`
	ExecutionID    string `gorm:"type:uuid;not null;uniqueIndex:uq_result_execution" json:"execution_id"` // 表 25：uq(execution_id)
	Verdict        string `gorm:"size:8;not null" json:"verdict"`                                         // PASS / FAIL
	Metrics        string `gorm:"type:jsonb;not null" json:"-"`
	Reconciliation string `gorm:"type:jsonb;not null" json:"-"`
	// Failure 失败原因原文（协议 v1.1 failure 块 {reason, message}）：仅 status=FAILED 的回传携带，其余 NULL。
	// 总体设计表 22 原文无此列——v1.1 扩展，待设计文档升版时同步。同 Metrics / Reconciliation：原文列不随模型序列化。
	Failure   *string   `gorm:"type:jsonb" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestResult) TableName() string { return "test_result" }
