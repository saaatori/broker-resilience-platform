package task

import (
	"testing"

	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// TestIntegrationRegisterIdempotent 注册幂等（v1.0.1）：同 hostname 复用 agent_id、
// 库存 token_hash 换新（旧 token 立即失效）、hostname 恰一行。
func TestIntegrationRegisterIdempotent(t *testing.T) {
	db := openIntegrationDB(t)
	svc := NewService(NewRepository(db), nil, nil, nil, nil)

	first, err := svc.Register("it-host", "1.0.0", 0)
	if err != nil {
		t.Fatalf("首次 Register: %v", err)
	}
	second, err := svc.Register("it-host", "1.1.0", 2)
	if err != nil {
		t.Fatalf("二次 Register: %v", err)
	}
	if first.AgentUUID != second.AgentUUID {
		t.Fatalf("同 hostname 应复用 agent_id: %s vs %s", first.AgentUUID, second.AgentUUID)
	}
	var rows []model.AgentInstance
	if err := db.Where("hostname = ?", "it-host").Find(&rows).Error; err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("hostname 应恰一行，实际 %d", len(rows))
	}
	if rows[0].TokenHash != HashToken(second.Token) {
		t.Error("库存 token_hash 应为新 token 哈希（旧 token 失效）")
	}
	if rows[0].MaxConcurrent != 2 || rows[0].Version != "1.1.0" || rows[0].Status != "online" {
		t.Errorf("行更新不符: %+v", rows[0])
	}
	// 旧 token 查询路径（FindAgentByUUID 按 uuid，token 失效经 token_hash 查验——day33 T3 中间件用）
	var byOldToken int64
	db.Model(&model.AgentInstance{}).Where("token_hash = ?", HashToken(first.Token)).Count(&byOldToken)
	if byOldToken != 0 {
		t.Error("旧 token 哈希应已不存在（被覆盖）")
	}
	if err := db.Where("hostname = ?", "no-such-host").First(&model.AgentInstance{}).Error; err != gorm.ErrRecordNotFound {
		t.Errorf("不存在的 hostname 应 RecordNotFound，实际 %v", err)
	}
}
