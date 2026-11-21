// register.go Agent 注册（M2 第 33 天，协议 v1.0.5 §2.1）。
// 幂等语义（v1.0.1）：同 hostname 重复注册=复用同一 agent_id、换发新 token（旧 token 立即失效）。
package task

import (
	"fmt"

	"github.com/google/uuid"
)

// RegisterResult 注册响应：agent_id 为对外 uuid；token 明文仅此一次返回。
type RegisterResult struct {
	AgentUUID string
	Token     string
}

// Register 注册 Agent：原子 upsert（uq(hostname)+ON CONFLICT）——并发同 hostname 收敛 1 行、
// 复用既有 agent_id、换发新 token（旧立即失效）；maxConcurrent<=0 时置 1。
func (s *Service) Register(hostname, version string, maxConcurrent int) (*RegisterResult, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	token, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	agentUUID, err := s.repo.UpsertAgent(uuid.NewString(), hostname, version, HashToken(token), maxConcurrent)
	if err != nil {
		return nil, fmt.Errorf("upsert agent: %w", err)
	}
	return &RegisterResult{AgentUUID: agentUUID, Token: token}, nil
}
