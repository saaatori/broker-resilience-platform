package task

import (
	"testing"
)

// TestGenerateToken token 形态：64 位 hex、两次生成不同（熵足够）。
func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, _ := GenerateToken()
	if len(a) != 64 {
		t.Errorf("token 应为 64 字符 hex，实际 %d", len(a))
	}
	if a == b {
		t.Error("两次生成的 token 不应相同")
	}
}

// TestHashToken 哈希确定性：同 token 同 hash、异 token 异 hash、输出 64 hex。
func TestHashToken(t *testing.T) {
	h1 := HashToken("abc")
	h2 := HashToken("abc")
	h3 := HashToken("abd")
	if h1 != h2 {
		t.Error("同 token 哈希应一致")
	}
	if h1 == h3 {
		t.Error("异 token 哈希应不同")
	}
	if len(h1) != 64 {
		t.Errorf("sha256 hex 应 64 字符，实际 %d", len(h1))
	}
}

func TestRegister(t *testing.T) {
	t.Run("新 hostname 生成新身份", func(t *testing.T) {
		repo := newFakeTaskRepo()
		svc := NewService(repo, nil, nil, nil, nil)
		res, err := svc.Register("host-a", "1.0.0", 0)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if res.AgentUUID == "" || res.Token == "" {
			t.Fatalf("agent_id 与 token 均应非空: %+v", res)
		}
		got := repo.agents[res.AgentUUID]
		if got == nil {
			t.Fatal("注册后应落 agent_instance 行")
		}
		if got.TokenHash != HashToken(res.Token) {
			t.Errorf("库存应为 token 哈希，want %s got %s", HashToken(res.Token), got.TokenHash)
		}
		if got.MaxConcurrent != 1 {
			t.Errorf("max_concurrent 缺省应 1，实际 %d", got.MaxConcurrent)
		}
		if got.Status != "online" {
			t.Errorf("注册后应 online，实际 %s", got.Status)
		}
	})
	t.Run("同 hostname 复用 agent_id 换 token", func(t *testing.T) {
		repo := newFakeTaskRepo()
		svc := NewService(repo, nil, nil, nil, nil)
		first, err := svc.Register("host-a", "1.0.0", 2)
		if err != nil {
			t.Fatalf("首次 Register: %v", err)
		}
		second, err := svc.Register("host-a", "1.1.0", 3)
		if err != nil {
			t.Fatalf("二次 Register: %v", err)
		}
		if second.AgentUUID != first.AgentUUID {
			t.Fatalf("同 hostname 应复用 agent_id：first=%s second=%s", first.AgentUUID, second.AgentUUID)
		}
		if second.Token == first.Token {
			t.Error("重复注册应换发新 token")
		}
		got := repo.agents[first.AgentUUID]
		if got.TokenHash != HashToken(second.Token) {
			t.Error("库存 token_hash 应已更新为新 token 哈希（旧 token 立即失效）")
		}
		if got.Version != "1.1.0" || got.MaxConcurrent != 3 {
			t.Errorf("version/max_concurrent 应更新: %+v", got)
		}
	})
}
