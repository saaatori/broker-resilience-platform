// event_gate.go 事件幂等闸门与取消标志（M2 第 34 天，表 26）：
// exec:evt:{eid}:{seq} SETNX TTL 10min——events 不入 PG 的幂等去重源（Redis 故障 5xx 不降级）；
// exec:cancel:{eid}——DISPATCHED 态取消标志（v1.0.5 I-2：首条 STATUS 到达时读取，存在则转 CANCELLING）。
package task

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// evtDedupTTL 幂等去重窗口（表 26：10min——有界窗口，窗口外重复可能重发，消费者按 seq 去重）。
const evtDedupTTL = 10 * time.Minute

// eventGate 事件闸门抽象（单测 fake 可替）。
type eventGate interface {
	// TryClaimSeq SETNX 幂等闸门：false=该 (eid,seq) 已见过（重复回传，静默去重）。
	TryClaimSeq(eid string, seq int64) (bool, error)
	// CancelFlag 读取取消标志（读取故障由调用方记日志降级为无标志——取消闭环有心跳兜底）。
	CancelFlag(eid string) (bool, error)
	// SetCancelFlag 写取消标志（day41 取消端点；DISPATCHED 专用——RUNNING 走任务态 CANCELLING）。
	// TTL 10min：正常路径 result 即删（RemoveCancelFlag）；清扫路径不删，TTL 兜底防累积
	//（表 26 名义"TTL=执行结束"，与 exec:evt 同取有界窗口——记档 day41）。
	SetCancelFlag(eid string) error
	// RemoveCancelFlag 执行结束时删 exec:cancel:{eid}（表 26 TTL=执行结束；失败仅告警）。
	RemoveCancelFlag(eid string) error
}

// RedisEventGate go-redis 实现（main 装配）。
type RedisEventGate struct{ client *redis.Client }

// NewRedisEventGate 构造。
func NewRedisEventGate(client *redis.Client) *RedisEventGate { return &RedisEventGate{client: client} }

func (g *RedisEventGate) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// TryClaimSeq SETNX exec:evt:{eid}:{seq}，值 "1"，TTL 10min。
func (g *RedisEventGate) TryClaimSeq(eid string, seq int64) (bool, error) {
	ctx, cancel := g.ctx()
	defer cancel()
	ok, err := g.client.SetNX(ctx, fmt.Sprintf("exec:evt:%s:%d", eid, seq), "1", evtDedupTTL).Result()
	if err != nil {
		return false, fmt.Errorf("redis SETNX exec:evt: %w", err) // 5xx 不降级（协议 §2.4 v1.0.4/05）
	}
	return ok, nil
}

// CancelFlag EXISTS exec:cancel:{eid}。
func (g *RedisEventGate) CancelFlag(eid string) (bool, error) {
	ctx, cancel := g.ctx()
	defer cancel()
	n, err := g.client.Exists(ctx, "exec:cancel:"+eid).Result()
	if err != nil {
		return false, fmt.Errorf("redis EXISTS exec:cancel: %w", err)
	}
	return n > 0, nil
}

// SetCancelFlag SET exec:cancel:{eid} "1" EX 10min（接口注释记档口径）。
func (g *RedisEventGate) SetCancelFlag(eid string) error {
	ctx, cancel := g.ctx()
	defer cancel()
	return g.client.Set(ctx, "exec:cancel:"+eid, "1", evtDedupTTL).Err()
}

// RemoveCancelFlag DEL exec:cancel:{eid}（表 26：执行结束）。
func (g *RedisEventGate) RemoveCancelFlag(eid string) error {
	ctx, cancel := g.ctx()
	defer cancel()
	return g.client.Del(ctx, "exec:cancel:"+eid).Err()
}
