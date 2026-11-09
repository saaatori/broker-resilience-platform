// BRP 控制面入口：加载配置 → 连接数据库 → 启动 HTTP 服务。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/saaatori/broker-resilience-platform/backend/internal/auth"
	"github.com/saaatori/broker-resilience-platform/backend/internal/storage"
)

// Config 服务配置，对应 configs/config.yaml。
type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`
	Database struct {
		DSN string `yaml:"dsn"`
	} `yaml:"database"`
	Auth struct {
		SeedUsername string `yaml:"seed_username"` // 种子管理员用户名，空则回退 admin
		SeedPassword string `yaml:"seed_password"` // 种子管理员密码，空则回退 admin123
	} `yaml:"auth"`
}

// loadConfig 读取并解析 YAML 配置，未填字段使用代码内默认值。
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	cfg.Server.Port = 8080 // 默认端口
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "configs/config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	db, err := storage.OpenPG(cfg.Database.DSN)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	// auth 模块装配（三件套惯例）：repository → service → 种子管理员 → handler。
	userRepo := auth.NewUserRepository(db)
	authSvc := auth.NewService(userRepo)
	if err := authSvc.SeedAdmin(cfg.Auth.SeedUsername, cfg.Auth.SeedPassword); err != nil {
		log.Fatalf("初始化种子管理员失败: %v", err)
	}

	r := gin.Default()

	authHandler := auth.NewHandler(authSvc)
	authHandler.RegisterRoutes(r.Group("/api/v1"))

	// /healthz 健康检查：连带探活数据库，供 Compose 探针与排障使用。
	r.GET("/healthz", func(c *gin.Context) {
		if sqlDB, err := db.DB(); err == nil {
			if err := sqlDB.Ping(); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_down", "error": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Format(time.RFC3339)})
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("BRP 控制面启动于 %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}
