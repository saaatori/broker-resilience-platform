// cleanup_queue.go 清理任务队列（M2 第 37 天）：
// Redis LIST `cleanup:queue`（RPUSH eid；day43 清扫消费端 RPOP 执行环境兜底清理）。
// 表 26 增补键记档（协议 §2.6 "Go 侧记入清理任务队列兜底"的落地形态；无 TTL——队列常驻，
// 消费端负责移除；入队失败仅告警，不阻断清扫状态收口）。
package task

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// cleanupQueueKey 队列键名。
const cleanupQueueKey = "cleanup:queue"

// cleanupQueue 清理队列抽象（单测 fake 可替）。
type cleanupQueue interface {
	Enqueue(eid string) error
}

// RedisCleanupQueue go-redis 实现。
type RedisCleanupQueue struct{ client *redis.Client }

// NewRedisCleanupQueue 构造。
func NewRedisCleanupQueue(client *redis.Client) *RedisCleanupQueue {
	return &RedisCleanupQueue{client: client}
}

// Enqueue RPUSH cleanup:queue eid。
func (q *RedisCleanupQueue) Enqueue(eid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.client.RPush(ctx, cleanupQueueKey, eid).Err(); err != nil {
		return fmt.Errorf("redis RPUSH %s: %w", cleanupQueueKey, err)
	}
	return nil
}
