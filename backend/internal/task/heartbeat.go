// heartbeat.go 心跳端点（M2 第 33 天，协议 v1.0.5 §2.2）：
// 双数组判定（lease_ok / cancel_requested）+ 租约刷新 + agent 存活信号。
package task

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// 执行子状态终态集（协议 §5：DONE/ABORTED 为止态）。
const (
	execStatusDone    = "DONE"
	execStatusAborted = "ABORTED"
)

// 执行子状态与任务态常量（状态机 v1.0.5 §4/§5；集中定义防散落字符串漂移）。
const (
	taskStatusCancelling = "CANCELLING"
	agentStatusOnline    = "online"
)

// heartbeatTTL agent:hb 缓存键 TTL（表 26：60s）。
const heartbeatTTL = 60 * time.Second

// execView 心跳判定的输入视图：执行行 + 所属任务状态（JOIN 一次取齐）。
// embedded：e.* 平铺进 Exec（否则 GORM 把嵌套模型当关联解析报错）。
type execView struct {
	Exec       model.TestExecution `gorm:"embedded"`
	TaskStatus string              `gorm:"column:task_status"`
}

// execVerdict 单个上报执行的三向判定。
type execVerdict int

const (
	execLeaseOK execVerdict = iota // 刷新租约并认可
	execCancel                     // 要求中止（取消优先，不刷新）
	execExclude                    // 两数组皆不入（差集语义）
)

// judgeExec v1.0.5 §2.2 lease_ok 判定分支序（存在性由调用方保证——查不到的 eid 不产生视图）：
// 归属他 Agent → 排除；执行终态（DONE/ABORTED）→ 排除；租约失效（≤now，严格 After）→ 排除（含 CANCELLING，
// v1.0.5 分支序）；任务 CANCELLING → 取消（互斥取消优先）；否则 → 认可。
func judgeExec(v execView, agentRowID int64, now time.Time) execVerdict {
	if v.Exec.AgentID != agentRowID {
		return execExclude
	}
	if v.Exec.Status == execStatusDone || v.Exec.Status == execStatusAborted {
		return execExclude
	}
	if v.Exec.LeaseUntil == nil || !v.Exec.LeaseUntil.After(now) {
		return execExclude
	}
	if v.TaskStatus == taskStatusCancelling {
		return execCancel
	}
	return execLeaseOK
}

// hbCache agent 存活缓存键抽象（表 26 agent:hb:{agent_id}，TTL 60s）。
// Redis 非阻断：写失败仅告警——租约本体在 PG（与 day34 events 幂等的 5xx 不降级形成对照，记档）。
type hbCache interface {
	SetHeartbeat(agentUUID string) error
}

// RedisHBCache go-redis 实现。
type RedisHBCache struct{ client *redis.Client }

// NewRedisHBCache 构造（main 装配传入 storage.OpenRedis 的客户端）。
func NewRedisHBCache(client *redis.Client) *RedisHBCache { return &RedisHBCache{client: client} }

// SetHeartbeat SETEX agent:hb:{uuid} 60。
func (c *RedisHBCache) SetHeartbeat(agentUUID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.client.Set(ctx, "agent:hb:"+agentUUID, time.Now().Unix(), heartbeatTTL).Err()
}

// Heartbeat 处理一次心跳：去重上报 → 批量取视图 → 逐个判定 → lease_ok 批量刷新租约（仅 lease_ok——
// 取消闭环限时 45s 由 day37 清扫收口，裁决记档）→ agent 存活信号（PG last_heartbeat_at + Redis 缓存键）。
// 既非 lease_ok 又非 cancel 的上报 eid 静默排除（差集语义=视同 410，由 Agent 自行处置）。
func (s *Service) Heartbeat(agent *model.AgentInstance, runningEIDs []string) (leaseOK, cancelRequested []string, err error) {
	seen := make(map[string]bool, len(runningEIDs))
	eids := make([]string, 0, len(runningEIDs))
	for _, eid := range runningEIDs { // 上报去重（协议未提，输入卫生）
		if !seen[eid] {
			seen[eid] = true
			eids = append(eids, eid)
		}
	}

	leaseOK = []string{}
	cancelRequested = []string{}
	if len(eids) > 0 {
		views, err := s.repo.FindExecutionsWithTaskStatus(eids)
		if err != nil {
			return nil, nil, err
		}
		byEID := make(map[string]execView, len(views))
		for _, v := range views {
			byEID[v.Exec.ExecutionID] = v
		}
		now := time.Now()
		for _, eid := range eids {
			v, ok := byEID[eid]
			if !ok {
				continue // 不存在 → 排除（差集）
			}
			switch judgeExec(v, agent.ID, now) {
			case execLeaseOK:
				leaseOK = append(leaseOK, eid)
			case execCancel:
				cancelRequested = append(cancelRequested, eid)
			}
		}
		if len(leaseOK) > 0 {
			if err := s.repo.RenewLeases(leaseOK, now.Add(LeaseTTL)); err != nil {
				return nil, nil, err
			}
		}
	}

	if err := s.repo.TouchAgentHeartbeat(agent.ID); err != nil {
		return nil, nil, err
	}
	if s.hbCache != nil {
		if err := s.hbCache.SetHeartbeat(agent.AgentID); err != nil {
			log.Printf("task: agent:hb 缓存键写入失败（不阻断，租约本体在 PG）: %v", err)
		}
	}
	return leaseOK, cancelRequested, nil
}
