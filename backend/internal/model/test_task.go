package model

import "time"

// TestTask 测试任务表（总体设计表 19）：一次"下单"即一行，params_snapshot 为合成后的参数快照
// （场景 default_params + 用户覆盖 + 平台注入，协议 v1.0.5 §6.1）。
// Status 为任务状态机七态之一（协议 v1.0.5 §4）：
// PENDING → DISPATCHED → RUNNING →（CANCELLING → CANCELLED ｜ COMPLETED ｜ FAILED），
// 枚举应用层校验（总体设计 5.3 约定）；创建时恒为 PENDING，流转自 day32 领取起。
// 外键列均为纯列（项目惯例：不用 GORM 关联约束）。
type TestTask struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	ScenarioID     int64     `gorm:"not null" json:"scenario_id"`
	CaseID         *int64    `json:"case_id,omitempty"` // 可空：缺省用场景默认参数
	InstanceID     int64     `gorm:"not null" json:"instance_id"`
	CreatedBy      int64     `gorm:"not null" json:"created_by"`
	ParamsSnapshot string    `gorm:"type:jsonb;not null" json:"-"`
	Status         string    `gorm:"size:16;not null;index" json:"status"` // 表 25：idx(status)，领取轮询热点
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestTask) TableName() string { return "test_task" }
