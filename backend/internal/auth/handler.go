package auth

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/saaatori/broker-resilience-platform/backend/internal/middleware"
)

// Handler HTTP 层：解析请求、调用 Service、组织响应。
type Handler struct {
	svc      *Service
	throttle *loginThrottle
}

// NewHandler 注入 Service 构造 Handler（默认限流口径：1 分钟内同一用户名 5 次失败）。
func NewHandler(svc *Service) *Handler {
	return newHandler(svc, newLoginThrottle(loginMaxFailures, loginWindow))
}

// newHandler 注入限流器（测试可注入时钟/口径）。
func newHandler(svc *Service, throttle *loginThrottle) *Handler {
	return &Handler{svc: svc, throttle: throttle}
}

// loginRequest 登录请求体；两字段必填，缺失或 JSON 畸形统一 400。
// username 上限 64 对齐 sys_user.username 列宽（总体设计表 14）：超长用户名不可能存在，直接 400——
// 顺带让限流表的键长有界（否则可达 MB 级的请求体整个成为 map 的键）。
type loginRequest struct {
	Username string `json:"username" binding:"required,max=64"`
	Password string `json:"password" binding:"required"`
}

// login POST /api/v1/auth/login：校验凭据，成功返回 JWT。
// 用户不存在与密码错误返回同一 401 报文，不暴露用户是否存在。
// 失败限流：同一用户名窗口内失败达上限 → 429 + Retry-After（此时正确口令也拒绝，窗口结束恢复）。
// 判定与预占额度原子完成（admit），口令校验后结算（settle）——并发请求无法整体绕过。
func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数不合法"})
		return
	}

	if wait, allowed := h.throttle.admit(req.Username); !allowed {
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录尝试过于频繁，请稍后再试"})
		return
	}

	token, err := h.svc.Login(req.Username, req.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		h.throttle.settle(req.Username, true) // 仅凭据错误计失败；数据库故障不计（那是服务端问题）
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
	case err != nil:
		h.throttle.settle(req.Username, false)
		log.Printf("auth: 登录内部错误: %v", err) // 错误链不含密码/密钥，可安全记录
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
	default:
		h.throttle.settle(req.Username, false)
		h.throttle.reset(req.Username) // 成功登录清除失败计数
		c.JSON(http.StatusOK, gin.H{"token": token})
	}
}

// RegisterRoutes 把本模块公开路由挂到 /api/v1 分组（无需登录）。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/login", h.login)
}

// RegisterProtectedRoutes 把需要登录的路由挂到受 JWTAuth 保护的分组。
func (h *Handler) RegisterProtectedRoutes(rg *gin.RouterGroup) {
	rg.GET("/me", h.me)
}

// me GET /api/v1/me：返回当前登录用户名（由 JWTAuth 中间件解析令牌后写入 context）。
func (h *Handler) me(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"username": c.GetString(middleware.UsernameKey)})
}
