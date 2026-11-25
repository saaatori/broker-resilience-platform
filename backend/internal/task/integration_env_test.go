package task

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/saaatori/broker-resilience-platform/backend/internal/storage"
)

// ---------- 集成测试环境（测试环境与开发环境分隔，2026-11-25 用户规则）----------
//
// 集成测试只跑在**专用测试环境**上。`task test:integration` 起一次性的 PostgreSQL 与 Redis 容器（只绑回环、
// 随机端口、跑完即删），通过下面三个环境变量指过来；测试代码只认这三个变量，不回退到开发环境。
// 此前 Redis 写死 localhost:6379 加开发口令，`BRP_TEST_DSN` 未设时的提示语给出的就是开发库的连接串——
// 照着填就会清空开发库，夹具场景和垃圾任务因此留在过开发库里（排障 #48）。

// testDBNamePrefix 专用测试库的库名前缀。
const testDBNamePrefix = "brp_test"

// isTestDatabase 库名是否属于专用测试库。
func isTestDatabase(name string) bool { return strings.HasPrefix(name, testDBNamePrefix) }

// testRedisOptions 测试 Redis 的连接参数：只认 BRP_TEST_REDIS_ADDR / BRP_TEST_REDIS_PASSWORD。
// skip=true 表示没有测试环境（只跑单测的场合）。设了 BRP_TEST_DSN 却没设 Redis 地址属于配置不完整，返回错误——
// 静默跳过会让依赖 Redis 的用例"看起来全绿"。
func testRedisOptions() (*redis.Options, bool, error) {
	addr := os.Getenv("BRP_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("BRP_TEST_DSN") != "" {
			return nil, false, errors.New("BRP_TEST_DSN 已设置但缺少 BRP_TEST_REDIS_ADDR：测试环境须成套配置（用 task test:integration）")
		}
		return nil, true, nil
	}
	return &redis.Options{Addr: addr, Password: os.Getenv("BRP_TEST_REDIS_PASSWORD")}, false, nil
}

// openIntegrationDB 集成测试库（实施规划 §11"少量集成测试"）：每个用例开头 TRUNCATE 自清。
// 没有测试环境（BRP_TEST_DSN 未设）即跳过，只跑单测的场合不红。
func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("BRP_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 BRP_TEST_DSN：集成测试只在专用测试环境上跑（task test:integration 会自动起一次性容器）")
	}
	// 成套检查：部分用例不碰 Redis，配置缺了一半要在这里就拦下，而不是等到碰 Redis 的用例才暴露
	if _, _, err := testRedisOptions(); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:         gormlogger.Discard,
		TranslateError: true, // 与生产 OpenPG 同口径（uq 冲突译码——day35 评审 I-1：测试连接漏配曾致穿透假象）
	})
	if err != nil {
		t.Fatalf("连测试库: %v", err)
	}
	// 护栏：只允许专用测试库，且必须排在任何写入之前——下面的 AutoMigrate 与 TRUNCATE 会动表里的全部数据
	var name string
	if err := db.Raw("select current_database()").Scan(&name).Error; err != nil {
		t.Fatalf("读取当前库名: %v", err)
	}
	if !isTestDatabase(name) {
		t.Fatalf("集成测试拒绝在库 %q 上运行：只允许库名以 %s 开头的专用测试库。本函数与各用例开头会 TRUNCATE 业务表，"+
			"对着开发库跑会清空开发数据；请用 task test:integration", name, testDBNamePrefix)
	}
	// 迁移清单与生产同源（storage.AutoMigrate）：此前这里只迁四表，结果三表靠开发库既有结构"顺带存在"——
	// 换一个空库（或生产新增列，如 v1.1 的 test_result.failure）集成测试就会建立在旧结构上。
	if err := storage.AutoMigrate(db); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := db.Exec("TRUNCATE test_task, test_execution, test_scenario, agent_instance, test_result, test_metric_summary, test_report RESTART IDENTITY").Error; err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	return db
}

// openTestRedis 测试 Redis 客户端（专用实例，用默认库 0），用例结束自动关闭。
func openTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	opts, skip, err := testRedisOptions()
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skip("未设置 BRP_TEST_REDIS_ADDR：需要专用测试 Redis（task test:integration 会自动起一次性容器）")
	}
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// 环境已声明却连不上是配置错误，不降级为跳过
		t.Fatalf("测试 Redis %s 不可达: %v", opts.Addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ---------- 护栏自身的单测 ----------

// TestIsTestDatabase 集成测试只允许跑在专用测试库上：库名以 brp_test 开头才算，
// 开发库 broker_resilience 以及任何别的名字一律不算。
func TestIsTestDatabase(t *testing.T) {
	cases := map[string]bool{
		"brp_test":          true,
		"brp_test_local":    true,
		"broker_resilience": false, // 开发库
		"postgres":          false,
		"BRP_TEST":          false, // 大小写不通融
		"xbrp_test":         false,
		"":                  false,
	}
	for name, want := range cases {
		if got := isTestDatabase(name); got != want {
			t.Errorf("isTestDatabase(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestTestRedisOptions 测试 Redis 的连接参数只认测试专用环境变量，不回退到开发 Redis。
func TestTestRedisOptions(t *testing.T) {
	t.Run("都没设置：跳过（没有测试环境，只跑单测）", func(t *testing.T) {
		t.Setenv("BRP_TEST_DSN", "")
		t.Setenv("BRP_TEST_REDIS_ADDR", "")
		t.Setenv("BRP_TEST_REDIS_PASSWORD", "")
		opts, skip, err := testRedisOptions()
		if !skip || err != nil || opts != nil {
			t.Fatalf("want (nil, skip=true, nil), got (%v, %v, %v)", opts, skip, err)
		}
	})
	t.Run("设了 DSN 却没设 Redis 地址：报错，不能静默跳过", func(t *testing.T) {
		t.Setenv("BRP_TEST_DSN", "host=127.0.0.1 port=15432 user=brp password=x dbname=brp_test sslmode=disable")
		t.Setenv("BRP_TEST_REDIS_ADDR", "")
		t.Setenv("BRP_TEST_REDIS_PASSWORD", "x")
		opts, skip, err := testRedisOptions()
		if skip || err == nil || opts != nil {
			t.Fatalf("want (nil, skip=false, err), got (%v, %v, %v)", opts, skip, err)
		}
	})
	t.Run("地址与口令只来自测试变量，使用专用实例的默认库", func(t *testing.T) {
		t.Setenv("BRP_TEST_DSN", "")
		t.Setenv("BRP_TEST_REDIS_ADDR", "127.0.0.1:16379")
		t.Setenv("BRP_TEST_REDIS_PASSWORD", "brp_it")
		opts, skip, err := testRedisOptions()
		if skip || err != nil || opts == nil {
			t.Fatalf("want (opts, false, nil), got (%v, %v, %v)", opts, skip, err)
		}
		if opts.Addr != "127.0.0.1:16379" || opts.Password != "brp_it" || opts.DB != 0 {
			t.Fatalf("连接参数应原样取自测试变量且 DB=0: addr=%q password=%q db=%d", opts.Addr, opts.Password, opts.DB)
		}
	})
}
