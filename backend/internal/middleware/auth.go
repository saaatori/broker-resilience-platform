// Package middleware 存放 Gin 中间件：当前为 JWT 鉴权，后续按需增加访问日志、recover 等。
package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// UsernameKey 是鉴权通过后写入 gin.Context 的用户名键。
// handler 侧统一经此常量取值，避免两包各写一份字符串导致漂移。
const UsernameKey = "username"

// TokenParser 令牌解析函数类型，由 auth 包实现后注入（middleware 不依赖 auth 包，依赖单向无环）。
type TokenParser func(tokenString string) (*jwt.RegisteredClaims, error)

// JWTAuth 要求请求携带 Authorization: Bearer <token>，校验通过把用户名写入 context。
// 缺头、格式不对、令牌无效/过期统一返回同一 401 报文（不区分失败原因，不给探测者信息）。
func JWTAuth(parse TokenParser) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录或令牌无效"})
			return
		}
		claims, err := parse(token)
		if err != nil {
			log.Printf("auth: 令牌校验失败: %v", err) // 记录原因便于排障，不含 token 本体；缺头分支属正常未登录流量，不记
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录或令牌无效"})
			return
		}
		if claims.Subject == "" {
			log.Print("auth: 令牌缺少 sub，拒绝")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录或令牌无效"})
			return
		}
		c.Set(UsernameKey, claims.Subject)
		c.Next()
	}
}
