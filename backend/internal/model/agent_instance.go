package model

import "time"

// AgentInstance 执行器实例表（总体设计表 16）。
// day32 提前迁移（原排 day33）：claim 请求的 agent_id(uuid) 需经本表解析为内部 id；
// register 端点与 X-Agent-Token 认证 day33 落地（当前 claim 为无 token 中间态，记档开发日志）。
// token_hash 存 agent_token 哈希（鉴权用，day33 消费）；capabilities 为能力声明 jsonb。
type AgentInstance struct {
	ID              int64      `gorm:"primaryKey" json:"id"`
	AgentID         string     `gorm:"type:uuid;not null;uniqueIndex:uq_agent_agent_id" json:"agent_id"` // 表 25：uq(agent_id)
	Hostname        string     `gorm:"size:255;not null;uniqueIndex:uq_agent_hostname" json:"hostname"`  // day33 评审 I-1：uq 支撑"同 hostname 复用 agent_id"幂等（v1.0.1）
	Version         string     `gorm:"size:32;not null" json:"version"`
	TokenHash       string     `gorm:"size:128;not null" json:"-"`
	Capabilities    *string    `gorm:"type:jsonb" json:"capabilities,omitempty"`
	MaxConcurrent   int        `gorm:"type:integer;default:1" json:"max_concurrent"`                      // 表 16 原文 int（type:integer 防内置常量吞）
	Status          string     `gorm:"size:16;not null" json:"status"`                                    // 在线/离线
	LastHeartbeatAt *time.Time `gorm:"index:idx_agent_last_heartbeat" json:"last_heartbeat_at,omitempty"` // 表 25：idx(last_heartbeat_at)
	CreatedAt       time.Time  `json:"created_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (AgentInstance) TableName() string { return "agent_instance" }
