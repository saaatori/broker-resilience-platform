package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestLoginThrottleViaHTTP HTTP 层闭环：连续错密 → 401；达上限后即使密码正确也 429（带 Retry-After）；
// 窗口过期后恢复 200。
func TestLoginThrottleViaHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	th, now := newTestThrottle()           // 窗口内最多 3 次失败
	h := newHandler(newTestService(t), th) // 账户 admin/admin123
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))

	post := func(username, password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	for i := 1; i <= 3; i++ {
		if w := post("admin", "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错密应 401，实际 %d", i, w.Code)
		}
	}
	w := post("admin", "admin123") // 密码正确也被限流（防继续试探）
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("达上限应 429，实际 %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if !strings.Contains(w.Body.String(), "频繁") {
		t.Fatalf("429 报文应说明原因: %s", w.Body.String())
	}

	*now = now.Add(time.Minute + time.Second)
	if w := post("admin", "admin123"); w.Code != http.StatusOK {
		t.Fatalf("窗口过期后正确账密应 200，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestLoginSuccessDoesNotConsumeBudget 正确登录不消耗失败额度（正常使用不会被自己的成功登录锁住）。
func TestLoginSuccessDoesNotConsumeBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	th, _ := newTestThrottle()
	h := newHandler(newTestService(t), th)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))
	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin123"})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次正确登录应 200，实际 %d", i+1, w.Code)
		}
	}
}

// TestLoginThrottleNotBypassedByConcurrency 并发绕过回归钉子（批修评审 I-1）：
// "判定是否限流"与"记一次失败"若是分离的两步，并发错密能在任何人记失败之前全部通过判定——限流形同虚设。
// 判定与预占必须原子完成：无论多少并发，一个窗口内真正被校验口令的错密至多 max 次。
func TestLoginThrottleNotBypassedByConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	th, _ := newTestThrottle() // 窗口内最多 3 次失败
	repo := newFakeRepo("admin123")
	repo.findDelay = 30 * time.Millisecond // 拉宽"判定→记失败"窗口，让竞态确定性地暴露
	h := newHandler(NewService(repo, ServiceConfig{JWTSecret: "test-secret", TokenTTL: time.Hour}), th)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))

	const n = 40
	codes := make(chan int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			codes <- w.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)

	unauthorized, limited := 0, 0
	for c := range codes {
		switch c {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	if unauthorized != 3 || limited != n-3 {
		t.Fatalf("并发 %d 个错密：应恰 3 个被校验(401)、其余 %d 个被限流(429)，实际 401=%d 429=%d", n, n-3, unauthorized, limited)
	}
}

// TestLoginDbFailureNotCounted 数据库故障不计失败额度（那是服务端问题，不该让正常用户被锁）。
func TestLoginDbFailureNotCounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	th, _ := newTestThrottle() // 窗口内最多 3 次失败
	repo := newFakeRepo("admin123")
	h := newHandler(NewService(repo, ServiceConfig{JWTSecret: "test-secret", TokenTTL: time.Hour}), th)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))

	post := func(password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	repo.findErr = errors.New("db down")
	for i := 0; i < 5; i++ { // 超过失败上限 3 次
		if w := post("admin123"); w.Code != http.StatusInternalServerError {
			t.Fatalf("数据库故障应 500，实际 %d", w.Code)
		}
	}
	repo.findErr = nil
	if w := post("admin123"); w.Code != http.StatusOK {
		t.Fatalf("数据库故障恢复后正确账密应 200（故障不计失败），实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestLoginRejectsOverlongUsername 超长用户名直接 400 且不进入限流表（键长有界，表不随请求体膨胀）。
func TestLoginRejectsOverlongUsername(t *testing.T) {
	gin.SetMode(gin.TestMode)
	th, _ := newTestThrottle()
	h := newHandler(newTestService(t), th)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))

	long := strings.Repeat("u", 65) // sys_user.username 列宽 64
	body, _ := json.Marshal(map[string]string{"username": long, "password": "x"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("65 字符用户名应 400，实际 %d", w.Code)
	}
	th.mu.Lock()
	n := len(th.fails)
	th.mu.Unlock()
	if n != 0 {
		t.Fatalf("被拒的用户名不得进入限流表，实际 %d 条", n)
	}
}
