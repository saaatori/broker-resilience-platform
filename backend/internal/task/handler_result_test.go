package task

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestPostResultInvalidDetailHidesLocalPath result 端点 400 的 detail 不得泄露构建机本地路径
// （v6 的错误首行含 file:///…/result.schema.json#）。建任务端点已有同类回归；协议 v1.1 新增的几种 400 来源
// （failure↔status 条件约束、faults 自洽规则、时间戳后置校验）都汇到同一个出口，此处逐来源钉住
// （2026-11-24 评审 I-3）。
func TestPostResultInvalidDetailHidesLocalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _, _, agent, _ := newResultFixture(t, "RUNNING")
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(AgentRowKey, agent); c.Next() }) // 鉴权中间件的落库效果
	NewHandler(svc).RegisterAgentAuthedRoutes(r.Group(""))

	cases := map[string][]byte{
		"failure 搭配 COMPLETED（schema 条件约束）": resultJSONExtra("COMPLETED", 1, `"failure":{"reason":"timeout","message":"x"}`),
		"faults 状态与时间戳矛盾（schema 自洽规则）":      resultJSONExtra("COMPLETED", 1, faultRecoveredJSON(faultTS("10"), `null`, faultTS("40"))),
		"faults 时间戳无时区（Go 后置校验）":            resultJSONExtra("COMPLETED", 1, faultJSON(`"2026-11-24T10:00:10"`)),
		"attempt 不匹配": resultJSON("COMPLETED", 2),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/executions/eid-1/result", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "file:///") || strings.Contains(w.Body.String(), ":\\\\") {
				t.Fatalf("400 报文泄露了本地路径: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"detail"`) || !strings.Contains(w.Body.String(), "结果不合法") {
				t.Fatalf("应返回统一文案并保留 detail（调用方要看得见哪里错）: %s", w.Body.String())
			}
		})
	}
}
