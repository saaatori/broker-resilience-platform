package model

import "time"

// TestExecution 执行记录表（总体设计表 20）：Agent 领取任务即创建一行（协议 v1.0.5 §2.3），
// (execution_id, attempt) 为结果回传幂等键（实施规划决策 4）。
// day31 仅建表，首行由 day32 claim 端点写入。
// Status 为执行子状态（协议 v1.0.5 §5 九值：CLAIMED→…→DONE/ABORTED）；
// LeaseUntil 为租约截止（心跳 15s / 租约 45s，决策 3；表 25 idx(lease_until) 支撑定时清扫）；
// AgentID 为纯列——agent_instance 表 day33 建模，不构成本表迁移依赖。
type TestExecution struct {
	ID          int64      `gorm:"primaryKey" json:"id"`
	ExecutionID string     `gorm:"type:uuid;not null;uniqueIndex:uq_execution_attempt,priority:1" json:"execution_id"` // 表 25：uq(execution_id, attempt)
	TaskID      int64      `gorm:"not null;index:idx_execution_task" json:"task_id"`                                    // 表 25：idx(task_id)
	Attempt     int        `gorm:"type:integer;not null;uniqueIndex:uq_execution_attempt,priority:2" json:"attempt"`   // 从 1 递增（表 20 原文 int；GORM 的 type:int 会被内置 DataType 常量吞掉，须写 integer）
	AgentID     int64      `gorm:"not null" json:"agent_id"`
	Status      string     `gorm:"size:16;not null" json:"status"`
	LeaseUntil  *time.Time `gorm:"index:idx_execution_lease" json:"lease_until,omitempty"` // 表 25：idx(lease_until)
	ExitReason  *string    `gorm:"size:32" json:"exit_reason,omitempty"`                   // lease_lost / cancelled / timeout / error 等
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestExecution) TableName() string { return "test_execution" }
