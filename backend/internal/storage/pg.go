// Package storage 负责数据库与缓存的连接、初始化与迁移。
package storage

import (
	"fmt"
	"log"
	"os"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// OpenPG 按DSN 连接 PostgreSQL，完成连接池设置并执行自动迁移。
// M0 阶段使用 AutoMigrate 快速起步；表结构稳定后演进为版本化迁移脚本。
func OpenPG(dsn string) (*gorm.DB, error) {
	// 开发期打印 SQL 便于排查；ParameterizedQueries 只打语句、不内嵌参数值——
	// broker_instance 的 Fernet 密文与将来任何凭据类绑定值都不会落进服务日志。
	sqlLog := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Info,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
		},
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: sqlLog,
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

	// 新增表时在此追加模型，如 &model.AgentInstance{}
	if err := db.AutoMigrate(&model.SysUser{}, &model.BrokerInstance{}); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	return db, nil
}
