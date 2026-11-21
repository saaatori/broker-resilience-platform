package task

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// newAuthTestEngine 构造挂 AgentAuth + Agent 路由的测试引擎（fake repo 真服务）。
func newAuthTestEngine(repo *fakeTaskRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(NewService(repo, nil, nil, nil, nil))
	agentAuthed := r.Group("/api/v1", AgentAuth(repo.FindByTokenHash))
	h.RegisterAgentAuthedRoutes(agentAuthed)
	return r
}

// TestAgentAuthMiddleware 中间件四路（day33 评审 I-2：此前零自动化覆盖）。
func TestAgentAuthMiddleware(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		failErr error
		want    int
	}{
		{"未带 token → 401", "", nil, http.StatusUnauthorized},
		{"token 未命中 → 401", "unknown", nil, http.StatusUnauthorized},
		{"DB 故障 → 500（与 401 分流）", "any", errFailingCache, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeTaskRepo()
			repo.agentQueryErr = tc.failErr
			r := newAuthTestEngine(repo)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/tasks/claim", nil)
			if tc.token != "" {
				req.Header.Set("X-Agent-Token", tc.token)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("want %d got %d: %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

// TestAgentAuthPassesTokenOwner 好 token → 放行且注入归属行（引擎内 401 分支不再触发）。
func TestAgentAuthPassesTokenOwner(t *testing.T) {
	repo := newFakeTaskRepo()
	res, err := NewService(repo, nil, nil, nil, nil).Register("own-host", "1.0", 1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r := newAuthTestEngine(repo)
	// body agent_id 故意错报 → 归属校验 401（证明中间件放行后 handler 校验生效）
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/tasks/claim",
		strings.NewReader(`{"agent_id":"00000000-0000-0000-0000-00000000dead"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", res.Token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("冒报他人 agent_id 应 401，实际 %d", w.Code)
	}
	// 正确 body → 走到 service（fake 无 PENDING → 204，证明全链贯通）
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/tasks/claim",
		strings.NewReader(`{"agent_id":"`+res.AgentUUID+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Agent-Token", res.Token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("正确归属应贯通到 service（无任务 204），实际 %d: %s", w2.Code, w2.Body.String())
	}
}

// TestHeartbeatOwnershipViaHTTP heartbeat 归属校验同型（body.agent_id vs token 归属）。
func TestHeartbeatOwnershipViaHTTP(t *testing.T) {
	repo := newFakeTaskRepo()
	res, _ := NewService(repo, nil, nil, nil, nil).Register("hb-host", "1.0", 1)
	r := newAuthTestEngine(repo)

	post := func(body, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/heartbeat", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(`{"agent_id":"`+res.AgentUUID+`","running_executions":[]}`, res.Token); code != http.StatusOK {
		t.Fatalf("正确归属心跳应 200，实际 %d", code)
	}
	if code := post(`{"agent_id":"00000000-0000-0000-0000-00000000dead","running_executions":[]}`, res.Token); code != http.StatusUnauthorized {
		t.Fatalf("冒报他人 agent_id 心跳应 401，实际 %d", code)
	}
	// 旧 token（重新 register 换发后）→ 401（幂等副作用）
	res2, _ := NewService(repo, nil, nil, nil, nil).Register("hb-host", "1.1", 1)
	if res2.AgentUUID != res.AgentUUID {
		t.Fatal("同 hostname 应复用 agent_id")
	}
	if code := post(`{"agent_id":"`+res.AgentUUID+`","running_executions":[]}`, res.Token); code != http.StatusUnauthorized {
		t.Fatalf("旧 token 应失效 401，实际 %d", code)
	}
}

// compile-time：gorm 哨兵仍在使用（fake 层依赖）。
var _ = gorm.ErrRecordNotFound

// 用 model 防止仅测试文件引用时报 unused（agentFromContext 注入类型）。
var _ *model.AgentInstance
