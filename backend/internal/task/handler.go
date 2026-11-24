// handler.go HTTP 层：解析请求、调用 Service、组织响应（instance 模块三件套惯例）。
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saaatori/broker-resilience-platform/backend/internal/httpserver"
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
		detail = scrubSchemaURL(detail) // v6 错误首行含构建机路径，剥离（同 events 端点）
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
	rg.GET("/tasks/:id/events", h.streamTaskEvents)
}

// RegisterAgentRoutes 把 Agent 协议**公开**路由挂载（仅 register——首次无 token）。
func (h *Handler) RegisterAgentRoutes(rg *gin.RouterGroup) {
	rg.POST("/agent/register", h.registerAgent)
}

// RegisterAgentAuthedRoutes 把需 X-Agent-Token 的 Agent 路由挂到 AgentAuth 中间件组（main 装配）。
func (h *Handler) RegisterAgentAuthedRoutes(rg *gin.RouterGroup) {
	rg.POST("/agent/tasks/claim", h.claim)
	rg.POST("/agent/heartbeat", h.heartbeat)
	rg.POST("/executions/:eid/events", h.postEvent)
	rg.POST("/executions/:eid/result", h.postResult)
}

// postResult POST /api/v1/executions/:eid/result（X-Agent-Token）：结果回传（§2.5）。
// 成功/重复/尾随均 200（first-wins 幂等）；404 归属；400 schema/attempt；500 内部。
func (h *Handler) postResult(c *gin.Context) {
	agent, ok := agentFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	raw, err := c.GetRawData()
	if err != nil || len(raw) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体为空或读取失败"})
		return
	}
	if int64(len(raw)) > maxResultBodyBytes { // 独立上限：reconciliation 差集数组可达千条（M3 大丢失场景），64KB 会误杀（day35 评审 I-4）
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体过大（result 上限 4MB）"})
		return
	}
	switch err := h.svc.Result(agent, c.Param("eid"), raw); {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"status": "ok"}) // 协议 §2.5 v1.0.1：响应 200
	case errors.Is(err, ErrExecutionNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "执行不存在"})
	case errors.Is(err, ErrInvalidResult):
		detail := strings.TrimPrefix(err.Error(), ErrInvalidResult.Error()+": ")
		c.JSON(http.StatusBadRequest, gin.H{"error": "结果不合法（对照 result schema）", "detail": scrubSchemaURL(detail)})
	default:
		log.Printf("task: result 内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	}
}

// maxEventBodyBytes 单条事件体上限（日志型 payload 的输入卫生；协议未定，防御值记档）。
const maxEventBodyBytes = 64 << 10

// maxResultBodyBytes 结果体上限=平台通用上限（middleware.MaxBodyBytes，单一来源）：差集证据数组 M3 可达千条；
// 记档：M3 观察实际体量再议是否单独放宽（届时需同步 nginx client_max_body_size）。
const maxResultBodyBytes = middleware.MaxBodyBytes

// postEvent POST /api/v1/executions/:eid/events（X-Agent-Token）：事件上报（§2.4）。
// 成功与重复均为 202 空体；404 归属；400 schema/ts；500 闸门故障（Redis 不可用不降级）。
func (h *Handler) postEvent(c *gin.Context) {
	agent, ok := agentFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "agent token 无效"})
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体读取失败"})
		return
	}
	if len(raw) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体为空"})
		return
	}
	if len(raw) > maxEventBodyBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体过大（事件单条上限 64KB）"})
		return
	}
	switch err := h.svc.Event(agent, c.Param("eid"), raw); {
	case err == nil:
		c.Status(http.StatusAccepted) // 202：含重复静默与终态记录（§4 兜底）
	case errors.Is(err, ErrExecutionNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "执行不存在"})
	case errors.Is(err, ErrInvalidEvent):
		detail := strings.TrimPrefix(err.Error(), ErrInvalidEvent.Error()+": ")
		detail = scrubSchemaURL(detail) // v6 错误首行含 file:///… 构建机路径，剥离（M-6）
		c.JSON(http.StatusBadRequest, gin.H{"error": "事件不合法（对照 events schema）", "detail": detail})
	default:
		log.Printf("task: events 内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	}
}

// parseID 从路径参数解析任务 id；非法返回 false（已写响应）。
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "任务 id 不合法"})
		return 0, false
	}
	return id, true
}

// scrubSchemaURL 剥离 v6 错误信息中的 schema 资源 URL（含构建机绝对路径，不外泄）。
func scrubSchemaURL(detail string) string {
	if i := strings.Index(detail, "\n- at "); i >= 0 {
		return detail[i+1:] // 保留违规定位行，丢弃首行 "jsonschema validation failed with 'file://…'"
	}
	return detail
}

// streamTaskEvents GET /api/v1/tasks/:id/events（JWT）：SSE 订阅任务事件流（§2.4 前端面）。
// 断线不回放（事件无持久化源）——重连后以任务状态查询兜底（v1.0.5 M-3）。
func (h *Handler) streamTaskEvents(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if _, err := h.svc.FindTask(id); err != nil { // 任务不存在 404（存在性校验；单角色无越权面）
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	ch, cancel := h.svc.SubscribeTaskEvents(id)
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no") // nginx 不缓冲（compose 反代链路）
	// 解除本连接的写超时：SSE 是长连接，否则会在全局 WriteTimeout（60s）到点后被掐断
	if err := httpserver.StreamNoWriteDeadline(c.Writer); err != nil {
		log.Printf("task: SSE 解除写超时失败（流将在写超时后中断）: %v", err)
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	c.Stream(func(w io.Writer) bool {
		select {
		case ev, ok := <-ch:
			if !ok {
				return false
			}
			c.SSEvent(ev.Name, ev.Data)
			return true
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n") // SSE 注释行保活（代理超时防御）；写失败随流关闭
			return true
		case <-c.Request.Context().Done():
			return false
		}
	})
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
