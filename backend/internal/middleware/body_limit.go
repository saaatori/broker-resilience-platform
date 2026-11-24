// body_limit.go 通用请求体上限（代码走查 #9）：此前仅 events/result 两个端点各自设限，
// 登录/建任务等 JSON 端点可被任意大体积请求白读白解析。
package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxBodyBytes 平台通用请求体上限（4MB，取 result 回传上限——差集证据数组可达千条；
// events 另有 64KB 端点级上限，在 handler 内判定）。
const MaxBodyBytes int64 = 4 << 20

// BodyLimit 请求体上限中间件：
//   - Content-Length 已知且超限 → 直接 413，不进入 handler；
//   - 长度未知（chunked）→ 包 MaxBytesReader，读超限即报错，由 handler 映射为 400（不无限读）。
//
// 两点如实说明：①长度未知的超限体表现为 400（handler 读 body 失败）而非 413；
// ②经 gin 的 responseWriter 包装，MaxBytesReader 的"响应后关连接"不保证生效——上限保证的是
// "不会被无限读"，不是"连接一定被关"。
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge,
				gin.H{"error": fmt.Sprintf("请求体过大（上限 %d 字节）", maxBytes)})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
