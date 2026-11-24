// throttle.go 登录失败限流（代码走查 #9：此前登录端点可无限次在线猜口令）。
package auth

import (
	"sync"
	"time"
)

// 限流口径：同一用户名在窗口内失败达上限即拒绝，直到窗口结束。
const (
	loginMaxFailures = 5
	loginWindow      = time.Minute
)

// loginThrottle 按用户名的固定窗口失败计数（进程内；单副本部署，重启即清零）。
//
// 为什么按用户名而不是 IP：平台只有一个管理员账户，在线爆破的目标就是它——按用户名计数直接封住
// 这条路，且不依赖客户端地址的可信度（nginx 反代后，RemoteAddr 是 nginx；gin 默认的 ClientIP
// 信任所有代理、取 X-Forwarded-For 最左值，可被请求方伪造；要按 IP 计数须另行配置可信代理或
// 直接读 nginx 覆盖写入的 X-Real-IP，并假定 go 端口不对外直连）。
//
// 代价（账号锁定型防护的固有取舍）：知道用户名的人持续发错密，就能让该用户名一直登不进去——
// 窗口过期后再错 max 次即再次锁定，并非"至多锁 1 分钟"。如需消除，须改为 IP+用户名组合键，
// 前提是把真实客户端 IP 取对。
type loginThrottle struct {
	mu     sync.Mutex
	fails  map[string]*failureWindow
	max    int
	window time.Duration
	now    func() time.Time // 可注入（测试）
}

// failureWindow 一个用户名的窗口状态。
type failureWindow struct {
	count    int       // 窗口内已结算的失败数
	inflight int       // 已预占额度、尚未结算的尝试数（判定与记账之间在途的请求）
	until    time.Time // 窗口结束时刻
}

func newLoginThrottle(max int, window time.Duration) *loginThrottle {
	return &loginThrottle{
		fails:  map[string]*failureWindow{},
		max:    max,
		window: window,
		now:    time.Now,
	}
}

// admit 原子地判定并预占一次尝试额度：窗口内"已失败数 + 在途数"达上限即拒绝（返回剩余等待时长）。
// 判定与预占在同一次加锁内完成——并发请求不可能全部赶在任何人记账之前通过判定
// （分离的 check-then-act 会被并发整体绕过）。每个被接受的调用方必须随后恰好调用一次 settle。
func (t *loginThrottle) admit(username string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.purgeLocked(now)

	fw := t.fails[username]
	if fw != nil && !now.Before(fw.until) {
		fw.count, fw.until = 0, now.Add(t.window) // 窗口已过：失败数从头计（在途数保留）
	}
	if fw != nil && fw.count+fw.inflight >= t.max {
		return fw.until.Sub(now), false
	}
	if fw == nil {
		fw = &failureWindow{until: now.Add(t.window)}
		t.fails[username] = fw
	}
	fw.inflight++
	return 0, true
}

// settle 结算一次已预占的尝试：failed 为真计一次失败，为假不计（成功登录、数据库故障）。
func (t *loginThrottle) settle(username string, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fw, ok := t.fails[username]
	if !ok {
		return // 预占后条目已被成功登录清除；本次尝试无从结算（无害：少计一次失败）
	}
	fw.inflight--
	if failed {
		now := t.now()
		if !now.Before(fw.until) {
			fw.count, fw.until = 0, now.Add(t.window) // 校验耗到了窗口之外：计入新窗口
		}
		fw.count++
	}
	if fw.inflight == 0 && fw.count == 0 {
		delete(t.fails, username)
	}
}

// reset 登录成功：清除该用户名的失败计数（在途尝试的预占保留）。
func (t *loginThrottle) reset(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fw, ok := t.fails[username]
	if !ok {
		return
	}
	fw.count = 0
	if fw.inflight == 0 {
		delete(t.fails, username)
	}
}

// purgeLocked 回收已过期且无在途尝试的条目（表不随被 spray 的用户名无限增长）。调用方须持锁。
func (t *loginThrottle) purgeLocked(now time.Time) {
	for name, fw := range t.fails {
		if fw.inflight == 0 && !now.Before(fw.until) {
			delete(t.fails, name)
		}
	}
}
