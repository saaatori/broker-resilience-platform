package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// fakeRepo 是 userRepository 的内存实现，语义与真 repository 对齐：
// FindByUsername 查无此人返回 gorm.ErrRecordNotFound（Login 的错误折叠分支依赖它）。
// 测试因此不依赖数据库与容器。
type fakeRepo struct {
	users       map[string]*model.SysUser
	createCalls int   // 记录 Create 调用次数：map 覆盖语义与真库 uniqueIndex 报错语义不同，幂等断言靠它兜住
	findErr     error // 非 nil 时 FindByUsername 直接返回它（注入 DB 故障）
}

// newFakeRepo 预置一个 admin 用户；哈希用 MinCost 换速度（Login 的比对逻辑与 cost 无关）。
func newFakeRepo(adminPassword string) *fakeRepo {
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		panic("test bcrypt hash: " + err.Error())
	}
	return &fakeRepo{users: map[string]*model.SysUser{
		"admin": {Username: "admin", PasswordHash: string(hash)},
	}}
}

func (f *fakeRepo) Count() (int64, error) { return int64(len(f.users)), nil }

func (f *fakeRepo) Create(u *model.SysUser) error {
	f.createCalls++
	f.users[u.Username] = u
	return nil
}

func (f *fakeRepo) FindByUsername(username string) (*model.SysUser, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.users[username]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return u, nil
}

// newTestService 固定密钥与 TTL 的被测 Service（内置 admin/admin123）。
func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(newFakeRepo("admin123"), ServiceConfig{JWTSecret: "test-secret", TokenTTL: time.Hour})
}

// TestLogin 表驱动覆盖登录三分支（逐日计划第 7 天 spec 用例）。
// 正确用例做双向断言：token 非空且能被 ParseToken 解回 admin，防"永远报错也全绿"。
func TestLogin(t *testing.T) {
	svc := newTestService(t)
	tests := []struct {
		name     string
		username string
		password string
		wantErr  error
	}{
		{"正确账密签发可用令牌", "admin", "admin123", nil},
		{"错误密码", "admin", "wrong", ErrInvalidCredentials},
		{"不存在用户", "nosuch", "whatever", ErrInvalidCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := svc.Login(tt.username, tt.password)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Login(%q, ***) 错误 = %v, 期望 %v", tt.username, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Login(%q, ***) 意外出错: %v", tt.username, err)
			}
			claims, err := svc.ParseToken(token)
			if err != nil {
				t.Fatalf("ParseToken(签发结果) 出错: %v", err)
			}
			if claims.Subject != "admin" {
				t.Fatalf("token sub = %q, 期望 admin", claims.Subject)
			}
		})
	}

	// 第四分支（评审补充，spec 三用例之外）：FindByUsername 非 NotFound 错误（DB 故障）
	// 必须原样包装上抛，不得折叠为 ErrInvalidCredentials——否则 DB 挂了用户会看到 401 密码错误，误导排障。
	t.Run("数据库故障错误上抛", func(t *testing.T) {
		repo := newFakeRepo("admin123")
		repo.findErr = errors.New("connection refused")
		svc := NewService(repo, ServiceConfig{JWTSecret: "test-secret", TokenTTL: time.Hour})

		_, err := svc.Login("admin", "admin123")
		if err == nil {
			t.Fatal("DB 故障应返回错误，实际 nil")
		}
		if errors.Is(err, ErrInvalidCredentials) {
			t.Fatal("DB 故障被误折叠为凭据错误（handler 会错返回 401）")
		}
		if !errors.Is(err, repo.findErr) {
			t.Fatalf("错误应包装上抛原始错误, 实际: %v", err)
		}
	})
}

// TestSeedAdmin 覆盖种子管理员的两个分支：空库写入（密文入库）与已有用户幂等跳过。
func TestSeedAdmin(t *testing.T) {
	cfg := ServiceConfig{JWTSecret: "test-secret", TokenTTL: time.Hour}

	t.Run("空库写入密文种子管理员", func(t *testing.T) {
		repo := &fakeRepo{users: map[string]*model.SysUser{}}
		svc := NewService(repo, cfg)
		if err := svc.SeedAdmin("admin", "admin123"); err != nil {
			t.Fatalf("SeedAdmin 出错: %v", err)
		}
		u, ok := repo.users["admin"]
		if !ok {
			t.Fatal("未写入 admin")
		}
		// 强断言：不是"不等于明文"即可，而必须是 admin123 的有效 bcrypt 哈希（证明走了生产 HashPassword 路径）
		if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("admin123")); err != nil {
			t.Fatalf("PasswordHash 不是 admin123 的有效 bcrypt 哈希: %v", err)
		}
		if n, _ := repo.Count(); n != 1 {
			t.Fatalf("用户数 = %d, 期望 1", n)
		}
	})

	t.Run("已有用户幂等跳过", func(t *testing.T) {
		repo := newFakeRepo("admin123")
		svc := NewService(repo, cfg)
		if err := svc.SeedAdmin("admin", "another"); err != nil {
			t.Fatalf("SeedAdmin 出错: %v", err)
		}
		if n, _ := repo.Count(); n != 1 {
			t.Fatalf("重复写入，用户数 = %d, 期望 1", n)
		}
		if repo.createCalls != 0 {
			t.Fatalf("已有用户时仍调用了 Create %d 次，幂等守卫失效", repo.createCalls)
		}
		// 预置管理员的密码未被覆盖（若守卫丢失，"another" 会覆盖重建，此处比对即失败）
		if err := bcrypt.CompareHashAndPassword([]byte(repo.users["admin"].PasswordHash), []byte("admin123")); err != nil {
			t.Fatal("预置管理员密码被覆盖，幂等守卫失效")
		}
	})
}

// TestParseToken 表驱动覆盖解析侧六分支（第 5/6 天评审承诺的用例，含 alg=none 攻击向量）。
func TestParseToken(t *testing.T) {
	const secret = "test-secret"
	makeSvc := func(sec string, ttl time.Duration) *Service {
		return NewService(newFakeRepo("x"), ServiceConfig{JWTSecret: sec, TokenTTL: ttl})
	}
	svc := makeSvc(secret, time.Hour)

	tamper := func(tok string) string { // 翻转末位字符破坏签名
		rep := byte('A')
		if tok[len(tok)-1] == 'A' {
			rep = 'B'
		}
		return tok[:len(tok)-1] + string(rep)
	}

	tests := []struct {
		name    string
		token   func(t *testing.T) string
		wantErr bool
	}{
		{"有效令牌round-trip", func(t *testing.T) string {
			tok, err := svc.issueToken("admin")
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}, false},
		{"过期令牌", func(t *testing.T) string {
			tok, err := makeSvc(secret, -time.Hour).issueToken("admin")
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}, true},
		{"篡改签名", func(t *testing.T) string {
			tok, err := svc.issueToken("admin")
			if err != nil {
				t.Fatal(err)
			}
			return tamper(tok)
		}, true},
		{"错误密钥签发", func(t *testing.T) string {
			tok, err := makeSvc("other-secret", time.Hour).issueToken("admin")
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}, true},
		{"缺失exp", func(t *testing.T) string {
			// 用正确密钥正常签名，但 claims 不带 ExpiresAt——WithExpirationRequired 应拒绝
			tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "admin"}).
				SignedString([]byte(secret))
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}, true},
		{"alg=none攻击向量", func(t *testing.T) string {
			// 手工拼无签名三段式令牌，WithValidMethods 白名单应拒绝
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
			payload := base64.RawURLEncoding.EncodeToString(
				[]byte(fmt.Sprintf(`{"sub":"admin","exp":%d}`, time.Now().Add(time.Hour).Unix())))
			return header + "." + payload + "."
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := svc.ParseToken(tt.token(t))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseToken 应拒绝该令牌，实际通过: %+v", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseToken 意外失败: %v", err)
			}
			if claims.Subject != "admin" {
				t.Fatalf("sub = %q, 期望 admin", claims.Subject)
			}
			if !claims.ExpiresAt.Time.After(time.Now()) {
				t.Fatalf("exp = %v, 应在未来", claims.ExpiresAt.Time)
			}
		})
	}
}
