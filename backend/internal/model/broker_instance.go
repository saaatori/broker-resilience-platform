package model

import "time"

// BrokerInstance 中间件实例表（总体设计表 15）：被测 RabbitMQ 的连接配置。
// password_enc 为 Fernet 密文，接口永不回显（json:"-"）；
// env_meta / health 为 jsonb，M0 登记阶段均为 NULL（实验环境元数据与健康检查后续里程碑写入）。
type BrokerInstance struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:128;not null" json:"name"`
	Kind        string    `gorm:"size:16;not null" json:"kind"` // external | experimental
	Host        string    `gorm:"size:255;not null" json:"host"`
	AmqpPort    int       `gorm:"default:5672" json:"amqp_port"`
	MgmtPort    int       `gorm:"default:15672" json:"mgmt_port"`
	Vhost       string    `gorm:"size:128;default:/" json:"vhost"`
	Username    string    `gorm:"size:64;not null" json:"username"`
	PasswordEnc string    `gorm:"type:text;not null" json:"-"`
	EnvMeta     *string   `gorm:"type:jsonb" json:"env_meta,omitempty"`
	Health      *string   `gorm:"type:jsonb" json:"health,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (BrokerInstance) TableName() string { return "broker_instance" }
