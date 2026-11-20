package model

import "time"

// TestScenario 测试场景表（总体设计表 17）：场景键唯一（对应场景库目录名），
// default_params 是 params_snapshot 合成的基底（协议 v1.0.5 §6.1：day31 起以本表为 seed 来源）。
// DefaultParams 为 jsonb 字符串，模型层不解码；接口经 DTO 组装时再展开为对象（json:"-" 惯例）。
type TestScenario struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	Key           string    `gorm:"size:64;not null;uniqueIndex:uq_scenario_key" json:"key"` // 表 25：uq(key)
	Name          string    `gorm:"size:128;not null" json:"name"`
	Description   *string   `gorm:"type:text" json:"description,omitempty"`
	DefaultParams string    `gorm:"type:jsonb;not null" json:"-"`
	Enabled       bool      `gorm:"default:true" json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (TestScenario) TableName() string { return "test_scenario" }
