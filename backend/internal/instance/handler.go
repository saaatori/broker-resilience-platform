package instance

import (
	"errors"
	"log"
	"net/http"

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
}
