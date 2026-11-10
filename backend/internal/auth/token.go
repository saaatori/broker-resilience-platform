package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// issueToken 为指定用户签发 HS256 JWT。
// claims 只放 sub（用户名）/ iat / exp 三项，保持令牌最小；令牌解析（第 6 天中间件用）将来也落在本文件。
func (s *Service) issueToken(username string) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   username,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.TokenTTL)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}
