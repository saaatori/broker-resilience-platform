package auth

import (
	"testing"
	"time"
)

// newTestThrottle 可注入时钟的限流器（窗口内最多 3 次失败）。
func newTestThrottle() (*loginThrottle, *time.Time) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	th := newLoginThrottle(3, time.Minute)
	th.now = func() time.Time { return now }
	return th, &now
}

// fail 一次完整的失败尝试：预占额度 → 结算为失败。
func fail(th *loginThrottle, username string) {
	if _, allowed := th.admit(username); !allowed {
		panic("test precondition: attempt should be admitted")
	}
	th.settle(username, true)
}

func TestLoginThrottle(t *testing.T) {
	t.Run("未达上限不限流，达到即限流并给出剩余等待", func(t *testing.T) {
		th, _ := newTestThrottle()
		for i := 0; i < 2; i++ {
			fail(th, "admin")
			if wait, allowed := th.admit("admin"); !allowed {
				t.Fatalf("第 %d 次失败后不应限流（wait=%v）", i+1, wait)
			}
			th.settle("admin", false) // 撤销上面这次探测性预占
		}
		fail(th, "admin")
		wait, allowed := th.admit("admin")
		if allowed {
			t.Fatal("满 3 次失败应限流")
		}
		if wait <= 0 || wait > time.Minute {
			t.Fatalf("剩余等待应在 (0, 1m]，实际 %v", wait)
		}
	})

	t.Run("窗口过期自动解除且计数重置", func(t *testing.T) {
		th, now := newTestThrottle()
		for i := 0; i < 3; i++ {
			fail(th, "admin")
		}
		if _, allowed := th.admit("admin"); allowed {
			t.Fatal("前置：应处于限流")
		}
		*now = now.Add(time.Minute + time.Second)
		// 新窗口从头计：再失败 1 次不应立即限流
		fail(th, "admin")
		if _, allowed := th.admit("admin"); !allowed {
			t.Fatal("窗口过期后应解除，且新窗口计数应重置")
		}
	})

	t.Run("在途预占占用额度：未结算的尝试也算数", func(t *testing.T) {
		th, _ := newTestThrottle()
		for i := 0; i < 3; i++ {
			if _, allowed := th.admit("admin"); !allowed {
				t.Fatalf("第 %d 次预占应被接受", i+1)
			}
		}
		if _, allowed := th.admit("admin"); allowed {
			t.Fatal("已有 3 个在途尝试，第 4 个必须被拒——否则并发可整体绕过")
		}
	})

	t.Run("非失败的结算不计数且不留残余", func(t *testing.T) {
		th, _ := newTestThrottle()
		for i := 0; i < 5; i++ {
			th.admit("admin")
			th.settle("admin", false) // 数据库故障/成功：不计失败
		}
		if _, allowed := th.admit("admin"); !allowed {
			t.Fatal("非失败尝试不得消耗额度")
		}
		th.mu.Lock()
		n := len(th.fails)
		th.mu.Unlock()
		if n != 1 { // 仅剩上面这次探测性预占
			t.Fatalf("结算完且无失败的条目应被移除，实际 %d 条", n)
		}
	})

	t.Run("登录成功清除计数", func(t *testing.T) {
		th, _ := newTestThrottle()
		fail(th, "admin")
		fail(th, "admin")
		th.reset("admin")
		fail(th, "admin")
		if _, allowed := th.admit("admin"); !allowed {
			t.Fatal("成功登录后计数应清除")
		}
	})

	t.Run("各用户名相互独立", func(t *testing.T) {
		th, _ := newTestThrottle()
		for i := 0; i < 3; i++ {
			fail(th, "admin")
		}
		if _, allowed := th.admit("other"); !allowed {
			t.Fatal("一个用户名被限流不应影响其他用户名")
		}
	})

	t.Run("过期条目被回收（表不无限增长）", func(t *testing.T) {
		th, now := newTestThrottle()
		for _, name := range []string{"u1", "u2", "u3"} {
			fail(th, name)
		}
		*now = now.Add(2 * time.Minute)
		th.admit("fresh") // 触发回收
		th.mu.Lock()
		n := len(th.fails)
		th.mu.Unlock()
		if n != 1 {
			t.Fatalf("过期条目应被回收，仅剩 fresh，实际 %d 条", n)
		}
	})
}
