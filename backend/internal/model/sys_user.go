// Package model 存放全部 GORM 数据模型（对应总体设计 5.3 节 11 张表）。
package model

import "time"

// SysUser 用户表：首期硬编码管理员 + JWT 单角色（总体设计表 14）。
type SysUser struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:64;uniqueIndex;not null" json:"username"`
	PasswordHash string    `gorm:"size:256;not null" json:"-"` // json:"-" 确保接口永不回显密码哈希
	CreatedAt    time.Time `json:"created_at"`
}

// TableName 显式指定表名，避免 GORM 复数推断歧义。
func (SysUser) TableName() string { return "sys_user" }
