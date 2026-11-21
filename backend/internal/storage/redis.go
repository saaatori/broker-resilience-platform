// Package storage——Redis 连接（M2 第 33 天）：仅缓存类键（表 26 agent:hb 等），
// 租约本体在 PG（lease_until），Redis 故障不阻断心跳主链路（task.Service 侧告警继续）。
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// OpenRedis 按 addr+password 连接 Redis 并 Ping 验活（fail-fast：配置错误启动期暴露）。
func OpenRedis(addr, password string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis %s: %w", addr, err)
	}
	return rdb, nil
}
