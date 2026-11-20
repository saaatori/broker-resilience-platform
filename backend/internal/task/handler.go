// handler.go HTTP 层：解析请求、调用 Service、组织响应（instance 模块三件套惯例）。
package task

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
	case errors.Is(err, ErrUserNotFound):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"}) // 有效 JWT 却查无用户：数据异常，不暴露细节
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

// taskToResp 组装对外响应 DTO；快照串解码为对象（UseNumber——响应里 10000 不变 10000.0）。
func taskToResp(t *model.TestTask, scenarioKey string) gin.H {
	dec := json.NewDecoder(strings.NewReader(t.ParamsSnapshot))
	dec.UseNumber()
	var snapshot any
	if err := dec.Decode(&snapshot); err != nil {
		log.Printf("task: 快照解码失败（任务 %d）: %v", t.ID, err)
		snapshot = nil
	}
	return gin.H{
		"id":              t.ID,
		"status":          t.Status,
		"scenario_key":    scenarioKey,
		"instance_id":     t.InstanceID,
		"params_snapshot": snapshot,
		"created_at":      t.CreatedAt,
	}
}
