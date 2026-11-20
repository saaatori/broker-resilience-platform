// claim.go 原子领取（M2 第 32 天，协议 v1.0.5 §2.3）：agent 身份解析 + 结果组装。
// 事务与 SKIP LOCKED 在 repository.ClaimNextTask；本层不做业务之外的任何事。
package task

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// ErrAgentNotFound agent_id(uuid) 未注册（handler 映射 401——协议 401 处置=重新 register，
// 对未注册 Agent 正是正确动作；记档 day32 裁决）。
var ErrAgentNotFound = errors.New("agent not found")

// ClaimResult 一次成功领取的完整结果（handler 组装 claim 响应五键）。
type ClaimResult struct {
	Task        *model.TestTask
	Exec        *model.TestExecution
	ScenarioKey string
}

// Claim 原子领取：uuid 解析 agent → repo 事务领取。
// ErrNoTask / ErrClaimConflict 原样上抛（handler 分别 204 / 409）。
func (s *Service) Claim(agentUUID string) (*ClaimResult, error) {
	agent, err := s.repo.FindAgentByUUID(agentUUID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAgentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find agent: %w", err) // DB 故障保留底层原因，走 500+日志路径
	}

	task, exec, scenarioKey, err := s.repo.ClaimNextTask(agent.ID)
	if err != nil {
		return nil, err // ErrNoTask / ErrClaimConflict / 包装错误原样上抛
	}
	return &ClaimResult{Task: task, Exec: exec, ScenarioKey: scenarioKey}, nil
}
