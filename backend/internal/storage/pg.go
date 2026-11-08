// Package storage 负责数据库与缓存的连接、初始化与迁移。
package storage

import (
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// OpenPG 按DSN 连接 PostgreSQL，完成连接池设置并执行自动迁移。
// M0 阶段使用 AutoMigrate 快速起步；表结构稳定后演进为版本化迁移脚本。
func OpenPG(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info), // 开发期打印 SQL，便于学习与排查
	})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("unwrap sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// 新增表时在此追加模型，如 &model.BrokerInstance{}
	if err := db.AutoMigrate(&model.SysUser{}); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	return db, nil
}
