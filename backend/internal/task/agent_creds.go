// agent_creds.go Agent 凭据（M2 第 33 天）：token 生成与哈希。
// 口径记档：实例凭据用 Fernet（可解密回连），agent token 用哈希（单向——Go 侧只验不还原）。
package task

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateToken 生成 32B 随机 token 的 hex 形态（64 字符）。明文仅在 register 响应出现一次。
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HashToken token 的 SHA-256 hex（64 字符）——库存形态，查询键。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
