// Package instance 中间件实例管理：登记（M0 第 8 天）、连接检测与健康查看（第 9 天）。
// 三件套惯例：handler.go（HTTP 层）→ service.go（业务逻辑）→ repository.go（数据访问）。
package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/fernet/fernet-go"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// ErrInvalidKind kind 不在 external | experimental 枚举内。
var ErrInvalidKind = errors.New("invalid instance kind")

// ErrInstanceNotFound 实例 id 不存在。
var ErrInstanceNotFound = errors.New("instance not found")

// checkDialTimeout 单次连接检测的拨号超时。
const checkDialTimeout = 3 * time.Second

// kind 枚举值（总体设计表 15）。
const (
	KindExternal     = "external"     // 外部目标实例
	KindExperimental = "experimental" // 平台实验实例
)

// CheckResult 一次连接检测结果；同时作为 health jsonb 的存储结构（表 15：最近健康检查结果）。
type CheckResult struct {
	CheckedAt time.Time `json:"checked_at"`
	Reachable bool      `json:"reachable"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"` // 不可达时的失败原因（网络错误文本）
}

// ServiceConfig instance 服务配置，由 main 从 config.yaml 装配。
type ServiceConfig struct {
	EncryptKey *fernet.Key // Fernet 密钥（32 字节，base64url 编码），main 启动时校验
}

// instanceRepository 是 Service 依赖的最小数据访问接口（单测可换内存实现）。
type instanceRepository interface {
	Create(ins *model.BrokerInstance) error
	FindByID(id int64) (*model.BrokerInstance, error)
	FindAll() ([]model.BrokerInstance, error)
	Save(ins *model.BrokerInstance) error
}

// Service instance 业务逻辑层。
type Service struct {
	repo instanceRepository
	cfg  ServiceConfig
}

// NewService 注入 repository 与配置构造 Service。
func NewService(repo instanceRepository, cfg ServiceConfig) *Service {
	return &Service{repo: repo, cfg: cfg}
}

// EncryptPassword 用 Fernet 加密连接密码（每次密文不同：内含随机 IV）。
func (s *Service) EncryptPassword(plain string) (string, error) {
	enc, err := fernet.EncryptAndSign([]byte(plain), s.cfg.EncryptKey)
	if err != nil {
		return "", fmt.Errorf("fernet encrypt: %w", err)
	}
	return string(enc), nil
}

// DecryptPassword 解密 Fernet 密文。ttl=0：存储型密文不做时效校验（密码不是会话令牌）。
func (s *Service) DecryptPassword(enc string) (string, error) {
	plain := fernet.VerifyAndDecrypt([]byte(enc), 0, []*fernet.Key{s.cfg.EncryptKey})
	if plain == nil {
		return "", fmt.Errorf("fernet decrypt: 密文无效或密钥不匹配")
	}
	return string(plain), nil
}

// Register 登记中间件实例：应用默认值（总体设计表 15）→ 密码 Fernet 加密 → 入库。
// kind 空则 external，非法返回 ErrInvalidKind；端口 0 则 5672/15672；vhost 空则 "/"。
func (s *Service) Register(name, kind, host string, amqpPort, mgmtPort int, vhost, username, password string) (*model.BrokerInstance, error) {
	if kind == "" {
		kind = "external"
	}
	if kind != KindExternal && kind != KindExperimental {
		return nil, ErrInvalidKind
	}
	if amqpPort == 0 {
		amqpPort = 5672
	}
	if mgmtPort == 0 {
		mgmtPort = 15672
	}
	if vhost == "" {
		vhost = "/"
	}

	enc, err := s.EncryptPassword(password)
	if err != nil {
		return nil, err
	}
	ins := &model.BrokerInstance{
		Name:        name,
		Kind:        kind,
		Host:        host,
		AmqpPort:    amqpPort,
		MgmtPort:    mgmtPort,
		Vhost:       vhost,
		Username:    username,
		PasswordEnc: enc,
	}
	if err := s.repo.Create(ins); err != nil {
		return nil, fmt.Errorf("create broker_instance: %w", err)
	}
	return ins, nil
}

// CheckInstance 对实例的 amqp 端口做 TCP 拨号检测，并把结果写入 health（无论成败都落库）。
// 拨号失败是检查结果而非错误：返回 (ins, reachable=false 的结果, nil)；
// 只有实例不存在（ErrInstanceNotFound）与落库失败才是 error。
func (s *Service) CheckInstance(id int64) (*model.BrokerInstance, *CheckResult, error) {
	ins, err := s.repo.FindByID(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, ErrInstanceNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("find broker_instance: %w", err)
	}

	res := s.dial(ins)
	enc, err := json.Marshal(res)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal check result: %w", err)
	}
	str := string(enc)
	ins.Health = &str
	if err := s.repo.Save(ins); err != nil {
		return nil, nil, fmt.Errorf("save broker_instance health: %w", err)
	}
	return ins, res, nil
}

// ListInstances 返回全部实例（列表页用）。
func (s *Service) ListInstances() ([]model.BrokerInstance, error) {
	list, err := s.repo.FindAll()
	if err != nil {
		return nil, fmt.Errorf("find all broker_instance: %w", err)
	}
	return list, nil
}

// FindInstance 按 id 查询实例（health 端点用）。
func (s *Service) FindInstance(id int64) (*model.BrokerInstance, error) {
	ins, err := s.repo.FindByID(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInstanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find broker_instance: %w", err)
	}
	return ins, nil
}

// dial 对 host:amqp_port 发起一次 TCP 拨号并计时；失败原因记入 Error 字段。
func (s *Service) dial(ins *model.BrokerInstance) *CheckResult {
	start := time.Now()
	// JoinHostPort 而非 Sprintf 拼接：IPv6 字面量（如 ::1）需要方括号包裹才是合法地址
	addr := net.JoinHostPort(ins.Host, strconv.Itoa(ins.AmqpPort))
	conn, err := net.DialTimeout("tcp", addr, checkDialTimeout)
	latency := time.Since(start).Milliseconds()
	res := &CheckResult{CheckedAt: time.Now(), LatencyMs: latency}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	_ = conn.Close()
	res.Reachable = true
	return res
}
