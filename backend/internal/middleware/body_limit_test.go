package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newBodyLimitEngine 挂 BodyLimit 的测试引擎：/echo 回显读到的字节数，读失败回 400；
// invoked 记录 handler 是否被进入。
func newBodyLimitEngine(limit int64, invoked *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(limit))
	r.POST("/echo", func(c *gin.Context) {
		*invoked = true
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusBadRequest, "read error")
			return
		}
		c.String(http.StatusOK, "%d", len(body))
	})
	return r
}

func TestBodyLimit(t *testing.T) {
	const limit = 1 << 10

	t.Run("限内放行", func(t *testing.T) {
		invoked := false
		r := newBodyLimitEngine(limit, &invoked)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(make([]byte, limit))))
		if w.Code != http.StatusOK || !invoked {
			t.Fatalf("等于上限应放行: code=%d invoked=%v body=%s", w.Code, invoked, w.Body.String())
		}
	})

	t.Run("Content-Length 超限→413 且不进 handler", func(t *testing.T) {
		invoked := false
		r := newBodyLimitEngine(limit, &invoked)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(make([]byte, limit+1))))
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("超限应 413，实际 %d: %s", w.Code, w.Body.String())
		}
		if invoked {
			t.Fatal("超限请求不得进入 handler（避免白读白解析大体积）")
		}
	})

	t.Run("长度未知的超限体→handler 读取报错（不无限读）", func(t *testing.T) {
		invoked := false
		r := newBodyLimitEngine(limit, &invoked)
		// io.MultiReader 不暴露 Len：请求走 chunked，ContentLength=-1，拦在读取阶段
		chunk := bytes.NewReader(make([]byte, 512))
		body := io.NopCloser(io.MultiReader(chunk, bytes.NewReader(make([]byte, 4096))))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/echo", body))
		if w.Code != http.StatusBadRequest || !invoked {
			t.Fatalf("未知长度的超限体应读失败 400: code=%d invoked=%v body=%s", w.Code, invoked, w.Body.String())
		}
	})

	t.Run("无请求体的 GET 不受影响", func(t *testing.T) {
		invoked := false
		r := newBodyLimitEngine(limit, &invoked)
		r.GET("/sse", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sse", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET 应 200，实际 %d", w.Code)
		}
	})
}
