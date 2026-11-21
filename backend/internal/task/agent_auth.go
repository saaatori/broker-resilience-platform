// agent_auth.go X-Agent-Token 认证中间件（M2 第 33 天，协议 §1/§3）。
// 放 task 包而非 middleware：verify 依赖 task 的 repo 类型，依赖方向 task←middleware 会成环——记档。
package task

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// AgentRowKey 鉴权通过后注入 gin.Context 的 agent 行键（handler 经此取身份）。
const AgentRowKey = "agent_row"

// AgentAuth 校验 X-Agent-Token：sha256(token) 查 agent_instance.token_hash，
// 命中注入完整 agent 行；未带/未命中统一 401 报文（不区分原因，不给探测面）；
// DB 故障 500（与 401 区分——Agent 对 401 的处置是重新 register，故障期不该触发）。
func AgentAuth(verify func(tokenHash string) (*model.AgentInstance, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("X-Agent-Token")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
			return
		}
		agent, err := verify(HashToken(token))
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
			return
		}
		if err != nil {
			log.Printf("task: agent token 校验内部错误: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
			return
		}
		c.Set(AgentRowKey, agent)
		c.Next()
	}
}

// agentFromContext 取中间件注入的 agent 行；不在则 false（挂载遗漏时防御）。
func agentFromContext(c *gin.Context) (*model.AgentInstance, bool) {
	v, ok := c.Get(AgentRowKey)
	if !ok {
		return nil, false
	}
	agent, ok := v.(*model.AgentInstance)
	return agent, ok
}
