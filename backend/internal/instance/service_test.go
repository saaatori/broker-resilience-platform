package instance

import (
	"strings"
	"testing"

	"github.com/fernet/fernet-go"
)

func newCryptoService(t *testing.T) *Service {
	t.Helper()
	var key fernet.Key
	if err := key.Generate(); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	return NewService(nil, ServiceConfig{EncryptKey: &key})
}

// TestPasswordCrypto 钉死 Fernet 加解密正确性（登记接口的核心安全逻辑）。
func TestPasswordCrypto(t *testing.T) {
	t.Run("round-trip 且密文为 Fernet 格式", func(t *testing.T) {
		svc := newCryptoService(t)
		enc, err := svc.EncryptPassword("s3cret-pa55")
		if err != nil {
			t.Fatalf("EncryptPassword 出错: %v", err)
		}
		if !strings.HasPrefix(enc, "gAAAA") {
			t.Fatalf("密文无 Fernet 版本前缀: %q", enc[:10])
		}
		if enc == "s3cret-pa55" {
			t.Fatal("密文等于明文")
		}
		plain, err := svc.DecryptPassword(enc)
		if err != nil {
			t.Fatalf("DecryptPassword 出错: %v", err)
		}
		if plain != "s3cret-pa55" {
			t.Fatalf("解密结果 = %q, 期望原文", plain)
		}
	})

	t.Run("同明文两次加密密文不同（随机 IV）", func(t *testing.T) {
		svc := newCryptoService(t)
		a, _ := svc.EncryptPassword("same")
		b, _ := svc.EncryptPassword("same")
		if a == b {
			t.Fatal("两次加密密文相同，IV 未随机化")
		}
	})

	t.Run("错误密钥解不开", func(t *testing.T) {
		svc := newCryptoService(t)
		enc, _ := svc.EncryptPassword("s3cret-pa55")

		var otherKey fernet.Key
		if err := otherKey.Generate(); err != nil {
			t.Fatal(err)
		}
		other := NewService(nil, ServiceConfig{EncryptKey: &otherKey})
		if _, err := other.DecryptPassword(enc); err == nil {
			t.Fatal("异密钥解密应失败，实际成功")
		}
	})
}
