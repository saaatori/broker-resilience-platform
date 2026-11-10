package auth

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

// loginRequest 登录请求体；两字段必填，缺失或 JSON 畸形统一 400。
type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// login POST /api/v1/auth/login：校验凭据，成功返回 JWT。
// 用户不存在与密码错误返回同一 401 报文，不暴露用户是否存在。
func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}

	token, err := h.svc.Login(req.Username, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
	case err != nil:
		log.Printf("auth: 登录内部错误: %v", err) // 错误链不含密码/密钥，可安全记录
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		c.JSON(http.StatusOK, gin.H{"token": token})
	}
}

// RegisterRoutes 把本模块路由挂到 /api/v1 分组。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/login", h.login)
}
