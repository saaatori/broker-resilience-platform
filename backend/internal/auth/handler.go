package auth

import "github.com/gin-gonic/gin"

// Handler HTTP 层：解析请求、调用 Service、组织响应。
type Handler struct {
	svc *Service
}

// NewHandler 注入 Service 构造 Handler。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterRoutes 把本模块路由挂到 /api/v1 分组。
// 当前尚无端点；第 5 天接入 rg.POST("/auth/login", h.login)。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {}
