// BRP 控制面入口：加载配置 → 连接数据库 → 启动 HTTP 服务。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/fernet/fernet-go"

	"github.com/saaatori/broker-resilience-platform/backend/internal/auth"
	"github.com/saaatori/broker-resilience-platform/backend/internal/instance"
	"github.com/saaatori/broker-resilience-platform/backend/internal/middleware"
	"github.com/saaatori/broker-resilience-platform/backend/internal/storage"
	"github.com/saaatori/broker-resilience-platform/backend/internal/task"
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
		JWTSecret    string `yaml:"jwt_secret"`    // HS256 签名密钥，必填
		TokenTTL     string `yaml:"token_ttl"`     // 令牌有效期（Go duration 格式），空则 24h
	} `yaml:"auth"`
	Instance struct {
		EncryptKey string `yaml:"encrypt_key"` // 实例凭据 Fernet 密钥（32 字节 base64url），必填
	} `yaml:"instance"`
	Redis struct {
		Addr     string `yaml:"addr"`     // 例 localhost:6379（本机）/ redis:6379（容器网内）
		Password string `yaml:"password"` // compose redis --requirepass 同值
	} `yaml:"redis"`
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
	applyEnvOverrides(cfg)
	return cfg, nil
}

// applyEnvOverrides 用 BRP_* 环境变量覆盖配置（容器化部署用，见 backend/Dockerfile 与 platform.yml）。
// 仅覆盖已设置的键——本地开发不设这些变量时，行为与纯 config.yaml 完全一致。
func applyEnvOverrides(cfg *Config) {
	if v, ok := os.LookupEnv("BRP_SERVER_PORT"); ok {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		} else {
			log.Printf("BRP_SERVER_PORT=%q 非法，忽略（保持 %d）", v, cfg.Server.Port)
		}
	}
	if v, ok := os.LookupEnv("BRP_DATABASE_DSN"); ok {
		cfg.Database.DSN = v
	}
	if v, ok := os.LookupEnv("BRP_AUTH_SEED_USERNAME"); ok {
		cfg.Auth.SeedUsername = v
	}
	if v, ok := os.LookupEnv("BRP_AUTH_SEED_PASSWORD"); ok {
		cfg.Auth.SeedPassword = v
	}
	if v, ok := os.LookupEnv("BRP_AUTH_JWT_SECRET"); ok {
		cfg.Auth.JWTSecret = v
	}
	if v, ok := os.LookupEnv("BRP_AUTH_TOKEN_TTL"); ok {
		cfg.Auth.TokenTTL = v
	}
	if v, ok := os.LookupEnv("BRP_INSTANCE_ENCRYPT_KEY"); ok {
		cfg.Instance.EncryptKey = v
	}
	if v, ok := os.LookupEnv("BRP_REDIS_ADDR"); ok {
		cfg.Redis.Addr = v
	}
	if v, ok := os.LookupEnv("BRP_REDIS_PASSWORD"); ok {
		cfg.Redis.Password = v
	}
}

func main() {
	configPath := flag.String("config", "configs/config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// JWT 配置校验（fail-fast：先于连库，配置错误不被数据库错误掩盖）；TTL 空则默认 24h。
	if cfg.Auth.JWTSecret == "" || cfg.Auth.JWTSecret == "CHANGE_ME" {
		log.Fatalf("config.yaml 缺少 auth.jwt_secret 或仍为模板占位值 CHANGE_ME（本地用 openssl rand -hex 32 生成随机串）")
	}
	tokenTTL := 24 * time.Hour
	if cfg.Auth.TokenTTL != "" {
		if tokenTTL, err = time.ParseDuration(cfg.Auth.TokenTTL); err != nil {
			log.Fatalf("auth.token_ttl 解析失败（示例 24h / 2h）: %v", err)
		}
		if tokenTTL <= 0 {
			log.Fatalf("auth.token_ttl 必须为正时长，当前值 %q", cfg.Auth.TokenTTL)
		}
	}

	// 实例凭据加密密钥（fail-fast，与 jwt_secret 同口径）：空/占位/非法 base64 均拒绝启动。
	if cfg.Instance.EncryptKey == "" || cfg.Instance.EncryptKey == "CHANGE_ME" {
		log.Fatalf("config.yaml 缺少 instance.encrypt_key（本地生成：python -c \"import base64,os;print(base64.urlsafe_b64encode(os.urandom(32)).decode())\"）")
	}
	instanceKey, err := fernet.DecodeKey(cfg.Instance.EncryptKey)
	if err != nil {
		log.Fatalf("instance.encrypt_key 不是合法 Fernet 密钥（32 字节 base64url）: %v", err)
	}

	db, err := storage.OpenPG(cfg.Database.DSN)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	// Redis（M2 第 33 天）：缓存类键（表 26 agent:hb 等）；fail-fast 与 PG 同口径——
	// 配置错启动期暴露，运行期故障由调用侧告警降级（租约本体在 PG，不阻断心跳）。
	if cfg.Redis.Addr == "" {
		log.Fatalf("config.yaml 缺少 redis.addr（本机 localhost:6379；容器 redis:6379）")
	}
	rdb, err := storage.OpenRedis(cfg.Redis.Addr, cfg.Redis.Password)
	if err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}

	// auth 模块装配（三件套惯例）：repository → service → 种子管理员 → handler。
	userRepo := auth.NewUserRepository(db)
	authSvc := auth.NewService(userRepo, auth.ServiceConfig{
		JWTSecret: cfg.Auth.JWTSecret,
		TokenTTL:  tokenTTL,
	})
	if err := authSvc.SeedAdmin(cfg.Auth.SeedUsername, cfg.Auth.SeedPassword); err != nil {
		log.Fatalf("初始化种子管理员失败: %v", err)
	}

	r := gin.Default()

	authHandler := auth.NewHandler(authSvc)
	apiV1 := r.Group("/api/v1")
	authHandler.RegisterRoutes(apiV1) // 公开：/auth/login
	authed := apiV1.Group("", middleware.JWTAuth(authSvc.ParseToken))
	authHandler.RegisterProtectedRoutes(authed) // 受保护：/me

	// instance 模块装配：全部端点挂受保护组。
	instanceRepo := instance.NewRepository(db)
	instanceSvc := instance.NewService(instanceRepo, instance.ServiceConfig{EncryptKey: instanceKey})
	instance.NewHandler(instanceSvc).RegisterRoutes(authed) // 受保护：POST /instances

	// task 模块装配（M2 第 31 天）：schema 编译与场景 seed 均 fail-fast——
	// 副本破损/库异常宁可启动期崩溃，不静默放行非法快照（与 jwt_secret 哨兵同口径）。
	taskValidator, err := task.NewValidator()
	if err != nil {
		log.Fatalf("初始化 params_snapshot 校验器失败: %v", err)
	}
	eventsValidator, err := task.NewEventsValidator()
	if err != nil {
		log.Fatalf("初始化 events 校验器失败: %v", err)
	}
	resultValidator, err := task.NewResultValidator()
	if err != nil {
		log.Fatalf("初始化 result 校验器失败: %v", err)
	}
	taskHub := task.NewHub()
	taskSvc := task.NewService(task.NewRepository(db), task.ServiceDeps{
		Instances: instanceRepo, Users: userRepo, Validator: taskValidator,
		HB:     task.NewRedisHBCache(rdb),
		Events: eventsValidator, Result: resultValidator,
		Gate: task.NewRedisEventGate(rdb), Hub: taskHub,
		CleanupQueue: task.NewRedisCleanupQueue(rdb),
	})
	if err := taskSvc.SeedScenario(); err != nil {
		log.Fatalf("初始化内置场景失败: %v", err)
	}
	// 租约清扫器（第 37 天）：默认 45s 周期（与租约口径同值）；BRP_SWEEP_INTERVAL 可配
	//（Go duration，如 2s——验收编排用短周期观察"5s 内被清扫"）；进程退出随死（day40 端到端再议优雅退出）。
	sweepInterval := 45 * time.Second
	if v, ok := os.LookupEnv("BRP_SWEEP_INTERVAL"); ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			sweepInterval = d
		} else {
			log.Printf("BRP_SWEEP_INTERVAL=%q 非法，保持默认 %s", v, sweepInterval)
		}
	}
	go task.NewSweeper(taskSvc, sweepInterval).Loop()

	taskHandler := task.NewHandler(taskSvc)
	taskHandler.RegisterRoutes(authed)     // 受保护：POST /tasks
	taskHandler.RegisterAgentRoutes(apiV1) // Agent 公开：POST /agent/register（首次无 token）
	// X-Agent-Token 认证组（第 33 天收口 day32 的 claim 无 token 中间态）：claim（heartbeat 第 33 天 T4 加入）
	agentRepo := task.NewRepository(db)
	agentAuthed := apiV1.Group("", task.AgentAuth(agentRepo.FindByTokenHash))
	taskHandler.RegisterAgentAuthedRoutes(agentAuthed)

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
