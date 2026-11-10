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

// ParseToken 解析并校验 JWT，供 middleware 经参数注入使用。
// 格式错 / 签名错 / 算法不在白名单 / 缺 exp / 已过期 / iat 在未来一律返回错误。
// WithValidMethods 显式锁定 HS256（防 alg=none 与 HS-RS 混淆攻击）；
// WithExpirationRequired 拒绝不带 exp 的令牌（我们签发的令牌必带）；
// WithIssuedAt 校验签发时间不在未来。
func (s *Service) ParseToken(tokenString string) (*jwt.RegisteredClaims, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return nil, fmt.Errorf("parse jwt: %w", err)
	}
	return claims, nil
}
