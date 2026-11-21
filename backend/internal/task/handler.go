// handler.go HTTP 层：解析请求、调用 Service、组织响应（instance 模块三件套惯例）。
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saaatori/broker-resilience-platform/backend/internal/middleware"
	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// Handler HTTP 层。
type Handler struct {
	svc *Service
}

// NewHandler 注入 Service 构造 Handler。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// createTaskRequest 建任务请求体：场景键 + 被测实例 + 可选参数覆盖（顶层浅合并）。
type createTaskRequest struct {
	ScenarioKey string         `json:"scenario_key" binding:"required,max=64"`
	InstanceID  int64          `json:"instance_id" binding:"required,min=1"`
	Params      map[string]any `json:"params"` // 可选；数字经 gin 默认解码为 float64，域内值 marshal 无失真
}

// create POST /api/v1/tasks（需登录）：创建测试任务（PENDING）。
// 成功 201 返回任务 DTO；params_snapshot 由 jsonb 串解码为对象返回（UseNumber 保整数形态）。
func (h *Handler) create(c *gin.Context) {
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}
	username := c.GetString(middleware.UsernameKey) // JWTAuth 中间件写入；受保护组内必有，GetString 非 string 时零值

	t, err := h.svc.Create(req.ScenarioKey, req.InstanceID, req.Params, username)
	switch {
	case errors.Is(err, ErrScenarioNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": "场景不存在或未启用"})
	case errors.Is(err, ErrInstanceNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例不存在"})
	case errors.Is(err, ErrInvalidParams):
		// 哨兵前缀剥掉后即 schema 违规摘要（计划要求 400 附摘要：调用方看得见违规点，排障不必翻服务日志）
		detail := strings.TrimPrefix(err.Error(), ErrInvalidParams.Error()+": ")
		log.Printf("task: 参数快照校验失败: %s", detail)
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数快照不合法（对照 params_snapshot schema）", "detail": detail})
	case errors.Is(err, ErrUserNotFound):
		log.Printf("task: 有效 JWT 但用户不存在（数据异常）: username=%q", username)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	case err != nil:
		log.Printf("task: 创建内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		c.JSON(http.StatusCreated, taskToResp(t, req.ScenarioKey))
	}
}

// RegisterRoutes 把任务模块路由挂到受 JWT 保护的分组。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/tasks", h.create)
}

// RegisterAgentRoutes 把 Agent 协议**公开**路由挂载（仅 register——首次无 token）。
func (h *Handler) RegisterAgentRoutes(rg *gin.RouterGroup) {
	rg.POST("/agent/register", h.registerAgent)
}

// RegisterAgentAuthedRoutes 把需 X-Agent-Token 的 Agent 路由挂到 AgentAuth 中间件组（main 装配）。
func (h *Handler) RegisterAgentAuthedRoutes(rg *gin.RouterGroup) {
	rg.POST("/agent/tasks/claim", h.claim)
	rg.POST("/agent/heartbeat", h.heartbeat)
}

// heartbeatRequest 心跳请求体（协议 §2.2）。
type heartbeatRequest struct {
	AgentID           string   `json:"agent_id" binding:"required,uuid"`
	RunningExecutions []string `json:"running_executions"` // 可空数组
}

// heartbeat POST /api/v1/agent/heartbeat（X-Agent-Token）：刷新租约兼取消指令轮询。
// 200 {lease_ok, cancel_requested}；上报中既不在两数组的执行=差集语义（视同 410，Agent 自行处置）。
func (h *Handler) heartbeat(c *gin.Context) {
	agent, ok := agentFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	var req heartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}
	if req.AgentID != agent.AgentID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	leaseOK, cancel, err := h.svc.Heartbeat(agent, req.RunningExecutions)
	if err != nil {
		log.Printf("task: heartbeat 内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"lease_ok": leaseOK, "cancel_requested": cancel})
}

// registerAgentRequest 注册请求体（协议 §2.1）。
type registerAgentRequest struct {
	Hostname      string `json:"hostname" binding:"required,max=255"`
	Version       string `json:"version" binding:"required,max=32"`
	MaxConcurrent int    `json:"max_concurrent" binding:"omitempty,min=0"`
}

// registerAgent POST /api/v1/agent/register：注册/重注册换 token。
// 响应 200 {agent_id, agent_token}——token 明文仅此一次（协议 §2.1）。
func (h *Handler) registerAgent(c *gin.Context) {
	var req registerAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}
	res, err := h.svc.Register(req.Hostname, req.Version, req.MaxConcurrent)
	if err != nil {
		log.Printf("task: agent 注册内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agent_id": res.AgentUUID, "agent_token": res.Token})
}

// claimRequest Agent 领取请求体（协议 §2.3）。
type claimRequest struct {
	AgentID string `json:"agent_id" binding:"required,uuid"`
}

// claim POST /api/v1/agent/tasks/claim（X-Agent-Token）：原子领取（SKIP LOCKED）。
// 200 五键（task_id 为 bigint 十进制字符串，v1.0.5 口径）；无任务 204 空体；
// 未注册 agent 401（处置=重新 register）；领取冲突 409（防御性保留）。
// body.agent_id 与 token 归属不一致 → 401（防 A 的 token 冒报 B 的身份）。
func (h *Handler) claim(c *gin.Context) {
	agent, ok := agentFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	var req claimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}
	if req.AgentID != agent.AgentID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	res, err := h.svc.Claim(agent.AgentID)
	switch {
	case errors.Is(err, ErrNoTask):
		c.Status(http.StatusNoContent) // 204：SKIP LOCKED 正常路径失败方，空响应体
	case errors.Is(err, ErrAgentNotFound):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent 未注册"})
	case errors.Is(err, ErrClaimConflict):
		log.Printf("task: claim 防御性冲突（理论不可达路径）: %v", err) // 保留 task id 上下文（哨兵包装链里有）
		c.JSON(http.StatusConflict, gin.H{"error": "领取冲突，请退避后重试"})
	case err != nil:
		log.Printf("task: claim 内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		c.JSON(http.StatusOK, gin.H{
			"task_id":         strconv.FormatInt(res.Task.ID, 10),
			"execution_id":    res.Exec.ExecutionID,
			"attempt":         res.Exec.Attempt,
			"scenario_key":    res.ScenarioKey,
			"params_snapshot": decodeSnapshot(res.Task.ParamsSnapshot, "claim"),
		})
	}
}

// decodeSnapshot 快照串解码为对象（UseNumber——响应里 10000 不变 10000.0）；失败记日志返回 nil。
func decodeSnapshot(raw string, where string) any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		log.Printf("task: 快照解码失败（%s）: %v", where, err)
		return nil
	}
	return v
}

// taskToResp 组装对外响应 DTO。
func taskToResp(t *model.TestTask, scenarioKey string) gin.H {
	return gin.H{
		"id":              t.ID,
		"status":          t.Status,
		"scenario_key":    scenarioKey,
		"instance_id":     t.InstanceID,
		"params_snapshot": decodeSnapshot(t.ParamsSnapshot, fmt.Sprintf("任务 %d", t.ID)),
		"created_at":      t.CreatedAt,
	}
}
