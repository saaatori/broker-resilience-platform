package instance

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/fernet/fernet-go"
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// fakeRepo instanceRepository 的内存实现（语义对齐真 repository：
// FindByID 查无返回 gorm.ErrRecordNotFound；Save 覆盖写）。
type fakeRepo struct {
	nextID int64
	items  map[int64]*model.BrokerInstance
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{nextID: 1, items: map[int64]*model.BrokerInstance{}}
}

func (f *fakeRepo) Create(ins *model.BrokerInstance) error {
	ins.ID = f.nextID
	f.nextID++
	f.items[ins.ID] = ins
	return nil
}

func (f *fakeRepo) FindByID(id int64) (*model.BrokerInstance, error) {
	ins, ok := f.items[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return ins, nil
}

func (f *fakeRepo) Save(ins *model.BrokerInstance) error {
	f.items[ins.ID] = ins
	return nil
}

// newRepoService 构造带内存 repo 的被测 Service。
func newRepoService(t *testing.T) (*Service, *fakeRepo) {
	t.Helper()
	var key fernet.Key
	if err := key.Generate(); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	repo := newFakeRepo()
	return NewService(repo, ServiceConfig{EncryptKey: &key}), repo
}

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

// TestCheckInstance 覆盖连接检测三分支：可达 / 不可达（结果也落库）/ 实例不存在。
func TestCheckInstance(t *testing.T) {
	t.Run("可达端口", func(t *testing.T) {
		svc, _ := newRepoService(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		port := ln.Addr().(*net.TCPAddr).Port

		ins, err := svc.Register("t-ok", "", "127.0.0.1", port, 0, "", "u", "p")
		if err != nil {
			t.Fatal(err)
		}
		got, res, err := svc.CheckInstance(ins.ID)
		if err != nil {
			t.Fatalf("CheckInstance 出错: %v", err)
		}
		if !res.Reachable || res.Error != "" {
			t.Fatalf("应可达, 实际: %+v", res)
		}
		if got.Health == nil {
			t.Fatal("可达结果未写入 health")
		}
		var back CheckResult
		if err := json.Unmarshal([]byte(*got.Health), &back); err != nil {
			t.Fatalf("health 不是合法 JSON: %v", err)
		}
		if !back.Reachable {
			t.Fatal("health 回读 reachable 应为 true")
		}
	})

	t.Run("不可达端口也落库并带原因", func(t *testing.T) {
		svc, _ := newRepoService(t)
		// 先占一个端口拿到确定被关闭的端口号，再关闭
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		ins, err := svc.Register("t-bad", "", "127.0.0.1", port, 0, "", "u", "p")
		if err != nil {
			t.Fatal(err)
		}
		got, res, err := svc.CheckInstance(ins.ID)
		if err != nil {
			t.Fatalf("拨号失败不应成为 API 错误: %v", err)
		}
		if res.Reachable || res.Error == "" {
			t.Fatalf("应不可达且带原因, 实际: %+v", res)
		}
		if got.Health == nil {
			t.Fatal("不可达结果也应写入 health")
		}
	})

	t.Run("实例不存在", func(t *testing.T) {
		svc, _ := newRepoService(t)
		_, _, err := svc.CheckInstance(9999)
		if !errors.Is(err, ErrInstanceNotFound) {
			t.Fatalf("应返回 ErrInstanceNotFound, 实际: %v", err)
		}
	})
}

// FindAll 按 id 升序返回全部实例（对齐真 repository 语义）。
func (f *fakeRepo) FindAll() ([]model.BrokerInstance, error) {
	list := make([]model.BrokerInstance, 0, len(f.items))
	for i := int64(1); i < f.nextID; i++ {
		if ins, ok := f.items[i]; ok {
			list = append(list, *ins)
		}
	}
	return list, nil
}

// TestListInstances 覆盖列表语义：空库返回空切片（handler 据此输出 []）、多条按 id 升序。
func TestListInstances(t *testing.T) {
	t.Run("空库返回空切片", func(t *testing.T) {
		svc, _ := newRepoService(t)
		list, err := svc.ListInstances()
		if err != nil {
			t.Fatalf("ListInstances 出错: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("空库应返回空切片, 实际 %d 条", len(list))
		}
	})

	t.Run("多条按id升序", func(t *testing.T) {
		svc, _ := newRepoService(t)
		for _, name := range []string{"c", "a", "b"} {
			if _, err := svc.Register(name, "", "127.0.0.1", 0, 0, "", "u", "p"); err != nil {
				t.Fatal(err)
			}
		}
		list, err := svc.ListInstances()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 3 {
			t.Fatalf("应 3 条, 实际 %d", len(list))
		}
		for i := 1; i < len(list); i++ {
			if list[i-1].ID >= list[i].ID {
				t.Fatalf("未按 id 升序: [%d]=%d >= [%d]=%d", i-1, list[i-1].ID, i, list[i].ID)
			}
		}
	})
}
