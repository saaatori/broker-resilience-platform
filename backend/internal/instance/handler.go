package instance

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Handler HTTP 层：解析请求、调用 Service、组织响应。
type Handler struct {
	svc *Service
}

// NewHandler 注入 Service 构造 Handler。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// registerRequest 登记请求体；name/host/username/password 必填，
// 其余字段缺省时由 Service 按总体设计表 15 落默认值。
// max 对齐表 15 列宽（超长入库只会变成 PG 报错 500，不如入口 400）；
// 端口限定合法 TCP 范围（表 15 未写，属输入卫生，负数/越界端口无意义）。
type registerRequest struct {
	Name     string `json:"name" binding:"required,max=128"`
	Kind     string `json:"kind" binding:"omitempty,max=16"`
	Host     string `json:"host" binding:"required,max=255"`
	AmqpPort int    `json:"amqp_port" binding:"omitempty,min=1,max=65535"`
	MgmtPort int    `json:"mgmt_port" binding:"omitempty,min=1,max=65535"`
	Vhost    string `json:"vhost" binding:"omitempty,max=128"`
	Username string `json:"username" binding:"required,max=64"`
	Password string `json:"password" binding:"required"`
}

// register POST /api/v1/instances（需登录）：登记中间件实例。
// 成功 201 返回实例 JSON（模型 PasswordEnc 带 json:"-"，任何响应都不会带密码字段）。
func (h *Handler) register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}

	ins, err := h.svc.Register(req.Name, req.Kind, req.Host, req.AmqpPort, req.MgmtPort, req.Vhost, req.Username, req.Password)
	switch {
	case errors.Is(err, ErrInvalidKind):
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind 必须为 external 或 experimental"})
	case err != nil:
		log.Printf("instance: 登记内部错误: %v", err) // 错误链不含密码明文/密钥
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		c.JSON(http.StatusCreated, ins)
	}
}

// RegisterRoutes 把实例模块路由挂到受 JWT 保护的分组（本模块全部端点需登录）。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/instances", h.register)
	rg.POST("/instances/:id/check", h.check)
	rg.GET("/instances/:id/health", h.health)
}

// parseID 从路径参数解析实例 id；非法返回 false（已写响应）。
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例 id 不合法"})
		return 0, false
	}
	return id, true
}

// check POST /api/v1/instances/:id/check：TCP 拨号检测 amqp 端口并落库 health。
// 拨号失败是检查结果（200 + reachable=false + 原因），实例不存在才是 404。
func (h *Handler) check(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ins, res, err := h.svc.CheckInstance(id)
	switch {
	case errors.Is(err, ErrInstanceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
	case err != nil:
		log.Printf("instance: 检测内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		c.JSON(http.StatusOK, gin.H{
			"id":        ins.ID,
			"name":      ins.Name,
			"host":      ins.Host,
			"amqp_port": ins.AmqpPort,
			"health":    res,
		})
	}
}

// health GET /api/v1/instances/:id/health：回读最近一次检测结果（未检测过为 null）。
func (h *Handler) health(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ins, err := h.svc.FindInstance(id)
	switch {
	case errors.Is(err, ErrInstanceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
	case err != nil:
		log.Printf("instance: 健康查询内部错误: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		var parsed any // health 为 *string 存 JSON 文本，解析后以对象返回；nil → null
		if ins.Health != nil {
			if err := json.Unmarshal([]byte(*ins.Health), &parsed); err != nil {
				log.Printf("instance: health JSON 解析失败（实例 %d）: %v", ins.ID, err) // jsonb 列按理不会脏，记日志便于排查
				parsed = nil
			}
		}
		c.JSON(http.StatusOK, gin.H{"health": parsed})
	}
}
