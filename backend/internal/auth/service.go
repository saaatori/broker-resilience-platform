// Package auth 认证模块：种子管理员初始化（M0 第 4 天）与登录/JWT（第 5~6 天接入）。
// 三件套惯例：handler.go（HTTP 层）→ service.go（业务逻辑）→ repository.go（数据访问）。
package auth

import (
	"fmt"
	"log"

	"golang.org/x/crypto/bcrypt"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// userRepository 是 Service 依赖的最小数据访问接口，*UserRepository 天然满足；
// 第 7 天表驱动单测用内存假实现替换此接口即可，无需连库。
type userRepository interface {
	Count() (int64, error)
	Create(u *model.SysUser) error
}

// Service auth 业务逻辑层：密码哈希与种子管理员。
type Service struct {
	repo userRepository
}

// NewService 注入 repository 构造 Service。
func NewService(repo userRepository) *Service { return &Service{repo: repo} }

// HashPassword 生成 bcrypt 哈希（DefaultCost=10，本地开发与演示场景够用）。
func (s *Service) HashPassword(plain string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt generate: %w", err)
	}
	return string(hashed), nil
}

// SeedAdmin 启动时调用：库中无任何用户则写入种子管理员，已有用户则跳过（幂等，可安全重启）。
// username/password 为空时回退默认值 admin / admin123（仅限本地开发，配置中显式填写可覆盖）。
func (s *Service) SeedAdmin(username, password string) error {
	count, err := s.repo.Count()
	if err != nil {
		return fmt.Errorf("count sys_user: %w", err)
	}
	if count > 0 {
		log.Printf("auth: 已有 %d 个用户，跳过种子管理员", count)
		return nil
	}

	if username == "" {
		username = "admin"
	}
	if password == "" {
		password = "admin123"
		log.Print("auth: 未配置种子管理员密码，回退默认 admin123（仅限本地开发）")
	}

	hash, err := s.HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.repo.Create(&model.SysUser{Username: username, PasswordHash: hash}); err != nil {
		return fmt.Errorf("create seed admin: %w", err)
	}
	log.Printf("auth: 种子管理员 %q 已创建", username)
	return nil
}
