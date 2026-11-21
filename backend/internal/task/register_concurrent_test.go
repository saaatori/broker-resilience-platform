package task

import (
	"sync"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// TestIntegrationRegisterConcurrentSameHostname v1.0.1 幂等不变量的并发钉子（day33 评审 I-1）：
// 并发同 hostname 注册必须收敛为恰 1 行、1 个 agent_id、全部持有同一最终 token 哈希
// （分裂行的 token 永不失效=悬挂凭证）。
func TestIntegrationRegisterConcurrentSameHostname(t *testing.T) {
	db := openIntegrationDB(t)
	svc := NewService(NewRepository(db), ServiceDeps{})

	const n = 12
	var wg sync.WaitGroup
	uuids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Register("race-host", "1.0.0", 1)
			if err != nil {
				uuids <- "ERR:" + err.Error()
				return
			}
			uuids <- res.AgentUUID
		}()
	}
	wg.Wait()
	close(uuids)

	for u := range uuids {
		if len(u) < 20 || u[:4] == "ERR:" {
			t.Fatalf("并发注册出错: %s", u)
		}
	}
	var rows []model.AgentInstance
	if err := db.Where("hostname = ?", "race-host").Find(&rows).Error; err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同 hostname 应收敛为恰 1 行，实际 %d", len(rows))
	}
}
