package task

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/saaatori/broker-resilience-platform/backend/internal/middleware"
)

// TestCreateInvalidParamsDetailHidesLocalPath 建任务 400 的 detail 不得泄露构建机本地路径
// （v6 的错误首行含 file:///…/params_snapshot.schema.json#）——events 端点 day34 已修，此处是漏网的同类。
func TestCreateInvalidParamsDetailHidesLocalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newFixture(t)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.UsernameKey, "admin"); c.Next() })
	NewHandler(f.svc).RegisterRoutes(r.Group(""))

	body := `{"scenario_key":"normal_delivery","instance_id":3,"params":{"assertions":[{"metric":"bogus","op":">=","value":1}]}}`
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法断言应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "file:///") || strings.Contains(w.Body.String(), ":\\\\") {
		t.Fatalf("400 报文泄露了本地路径: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "metric") {
		t.Fatalf("detail 仍应保留违规定位（调用方要看得见哪里错）: %s", w.Body.String())
	}
}
