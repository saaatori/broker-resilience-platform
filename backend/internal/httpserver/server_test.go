package httpserver

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestDefaultTimeouts 默认超时口径钉子：四项均非零（此前 r.Run 零超时——慢速连接可无限占用）。
func TestDefaultTimeouts(t *testing.T) {
	srv := New(":0", http.NewServeMux())
	want := Timeouts{ReadHeader: 10 * time.Second, Read: 30 * time.Second, Write: 60 * time.Second, Idle: 120 * time.Second}
	got := Timeouts{srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout}
	if got != want {
		t.Fatalf("默认超时口径漂移: want %v got %v", want, got)
	}
}

// TestStreamSurvivesWriteTimeout SSE 与全局超时的相容性（本包存在的理由）：
// WriteTimeout/ReadTimeout 短于流时长时——
//   - 处理器调用 StreamNoWriteDeadline：流完整送达（长连接不被掐断）；
//   - 不调用：流在写超时后被截断（证明全局超时确实生效，清除调用不是多余的）。
//
// 用真实 TCP（httptest.Server）+ gin 引擎：gin 的 responseWriter 包装须经 Unwrap 透传到连接。
func TestStreamSurvivesWriteTimeout(t *testing.T) {
	const frames = 8
	run := func(t *testing.T, clear bool) int {
		t.Helper()
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.GET("/sse", func(c *gin.Context) {
			if clear {
				if err := StreamNoWriteDeadline(c.Writer); err != nil {
					c.String(http.StatusInternalServerError, err.Error())
					return
				}
			}
			c.Header("Content-Type", "text/event-stream")
			for i := 0; i < frames; i++ {
				time.Sleep(100 * time.Millisecond) // 总时长 800ms，远超下方的读/写超时
				if _, err := fmt.Fprintf(c.Writer, "data: %d\n\n", i); err != nil {
					return // 写失败：连接已被超时关闭
				}
				c.Writer.Flush()
			}
		})
		// 文档写法：NewUnstartedServer 后改 Config 字段再 Start；字段值取自真实构造器（映射逻辑被测）
		ts := httptest.NewUnstartedServer(r)
		configured := NewWithTimeouts(ts.Config.Addr, r, Timeouts{
			ReadHeader: time.Second, Read: 150 * time.Millisecond, Write: 250 * time.Millisecond,
		})
		ts.Config.ReadHeaderTimeout = configured.ReadHeaderTimeout
		ts.Config.ReadTimeout = configured.ReadTimeout
		ts.Config.WriteTimeout = configured.WriteTimeout
		ts.Config.IdleTimeout = configured.IdleTimeout
		ts.Start()
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/sse")
		if err != nil {
			t.Fatalf("GET /sse: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		n := 0
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				n++
			}
		}
		return n
	}

	t.Run("清除写超时后流完整送达", func(t *testing.T) {
		if n := run(t, true); n != frames {
			t.Fatalf("SSE 流应完整送达 %d 帧，实际 %d（被全局超时掐断）", frames, n)
		}
	})
	t.Run("不清除则被写超时截断", func(t *testing.T) {
		if n := run(t, false); n >= frames {
			t.Fatalf("未清除写超时的流应被截断，实际完整送达 %d 帧（全局写超时未生效？）", n)
		}
	})
}
