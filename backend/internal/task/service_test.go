package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// fixture 组装 Create 测试依赖：seed 场景（default_params=lab 定稿值）+ 实例 3 + 用户 admin。
type fixture struct {
	repo  *fakeTaskRepo
	users *fakeUserRepo
	svc   *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := newFakeTaskRepo()
	ins := &model.BrokerInstance{ID: 3, Name: "lab-broker"}
	user := &model.SysUser{ID: 1, Username: "admin"}
	users := &fakeUserRepo{users: map[string]*model.SysUser{"admin": user}}
	svc := NewService(repo, &fakeInstanceRepo{instances: map[int64]*model.BrokerInstance{3: ins}}, users, newTestValidator(t), nil)
	if err := svc.SeedScenario(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &fixture{repo: repo, users: users, svc: svc}
}

// snapshotMap 把落库快照串解回 map（UseNumber，保整数形态供断言）。
func snapshotMap(t *testing.T, task *model.TestTask) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(task.ParamsSnapshot))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("落库快照不是合法 JSON 对象: %v\n内容: %s", err, task.ParamsSnapshot)
	}
	return m
}

func TestCreate(t *testing.T) {
	// 用例① 无 params：快照=场景默认四键 + 注入 instance_ref
	t.Run("无覆盖时快照为默认值加 instance_ref", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, nil, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		m := snapshotMap(t, task)
		if m["message_count"] != json.Number("10000") {
			t.Errorf("message_count 应继承默认 10000，实际 %v", m["message_count"])
		}
		if m["instance_ref"] != "3" {
			t.Errorf("instance_ref 应注入字符串 \"3\"，实际 %v (%T)", m["instance_ref"], m["instance_ref"])
		}
	})

	// 用例② 覆盖 message_count：其余默认保留
	t.Run("顶层浅合并保留未覆盖键", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, map[string]any{"message_count": 1000}, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		m := snapshotMap(t, task)
		if m["message_count"] != json.Number("1000") {
			t.Errorf("message_count 应为覆盖值 1000，实际 %v", m["message_count"])
		}
		if m["publish_rate"] != json.Number("500") {
			t.Errorf("publish_rate 应保留默认 500，实际 %v", m["publish_rate"])
		}
	})

	// 用例③ 覆盖 fault 整对象
	t.Run("fault 覆盖整对象", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, map[string]any{
			"fault": map[string]any{"strategy": "consumer_kill", "inject_at_s": 10, "duration_s": 15},
		}, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		fault, ok := snapshotMap(t, task)["fault"].(map[string]any)
		if !ok {
			t.Fatal("快照应含 fault 对象")
		}
		if fault["strategy"] != "consumer_kill" || fault["duration_s"] != json.Number("15") {
			t.Errorf("fault 覆盖不符: %v", fault)
		}
	})

	// 用例④ 空 assertions vs 缺省（day25 裁决：显式空=vacuous PASS 保留；缺省=继承默认集）
	t.Run("显式空断言保留为空列表", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, map[string]any{"assertions": []any{}}, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := snapshotMap(t, task)["assertions"].([]any)
		if len(got) != 0 {
			t.Errorf("显式空断言应保留为 []，实际 %v", got)
		}
	})
	t.Run("缺省断言继承场景默认三条", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, nil, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := snapshotMap(t, task)["assertions"].([]any)
		if len(got) != 3 {
			t.Errorf("缺省应继承默认三条断言，实际 %d 条: %v", len(got), got)
		}
	})

	// 用例⑤ 平台管理键不透传（Review Focus 1）
	t.Run("平台管理键被清除或平台覆盖", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, map[string]any{
			"task_id": "FAKE", "execution_id": "FAKE", "instance_ref": "FAKE",
		}, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		m := snapshotMap(t, task)
		if _, exists := m["task_id"]; exists {
			t.Error("用户传入的 task_id 应被清除")
		}
		if _, exists := m["execution_id"]; exists {
			t.Error("用户传入的 execution_id 应被清除")
		}
		if m["instance_ref"] != "3" {
			t.Errorf("instance_ref 应被平台值覆盖为 \"3\"，实际 %v", m["instance_ref"])
		}
	})

	// 用例⑥⑦⑧ schema 负例 → ErrInvalidParams
	for name, override := range map[string]map[string]any{
		"metric 枚举外":   {"assertions": []any{map[string]any{"metric": "bogus", "op": ">=", "value": 1.0}}},
		"message_count 0": {"message_count": 0},
		"顶层未知键":      {"unknown_key": 1},
	} {
		t.Run("校验拒绝_"+name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Create("normal_delivery", 3, override, "admin")
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("应返回 ErrInvalidParams，实际 %v", err)
			}
		})
	}

	// 用例⑨⑩⑪ 引用负 → 对应哨兵
	t.Run("场景不存在", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.Create("no_such_scenario", 3, nil, "admin"); !errors.Is(err, ErrScenarioNotFound) {
			t.Fatalf("应返回 ErrScenarioNotFound，实际 %v", err)
		}
	})
	t.Run("禁用场景视为不存在", func(t *testing.T) {
		f := newFixture(t)
		s := f.repo.scenarios["normal_delivery"]
		s.Enabled = false
		if _, err := f.svc.Create("normal_delivery", 3, nil, "admin"); !errors.Is(err, ErrScenarioNotFound) {
			t.Fatalf("禁用场景应返回 ErrScenarioNotFound，实际 %v", err)
		}
	})
	t.Run("实例不存在", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.Create("normal_delivery", 999, nil, "admin"); !errors.Is(err, ErrInstanceNotFound) {
			t.Fatalf("应返回 ErrInstanceNotFound，实际 %v", err)
		}
	})

	// 用例⑫⑬ 落库形态与数值钉子（UseNumber，Review Focus 3）
	t.Run("落库 PENDING 且字段归属正确", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, map[string]any{"message_count": 1000}, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		stored := f.repo.tasks[0]
		if stored.Status != "PENDING" {
			t.Errorf("落库状态应 PENDING，实际 %q", stored.Status)
		}
		if stored.CreatedBy != 1 || stored.InstanceID != 3 || stored.ScenarioID == 0 {
			t.Errorf("外键归属不符: created_by=%d instance_id=%d scenario_id=%d", stored.CreatedBy, stored.InstanceID, stored.ScenarioID)
		}
		if task.Status != "PENDING" {
			t.Errorf("返回任务状态应 PENDING，实际 %q", task.Status)
		}
	})
	t.Run("快照整数形态不失真", func(t *testing.T) {
		f := newFixture(t)
		task, err := f.svc.Create("normal_delivery", 3, nil, "admin")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		m := snapshotMap(t, task)
		if m["message_count"] != json.Number("10000") {
			t.Errorf("message_count 应为 json.Number(\"10000\")（不经 float64 往返），实际 %#v", m["message_count"])
		}
	})
}

func TestCreateUserNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Create("normal_delivery", 3, nil, "ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("应返回 ErrUserNotFound，实际 %v", err)
	}
}

// TestCreateUserQueryFailureDistinct 用户查询的 DB 故障不得吞为 ErrUserNotFound——
// 前者是 500（须留日志线索），后者是数据异常哨兵；混吞会让排障无线索（day31 评审 I-1）。
func TestCreateUserQueryFailureDistinct(t *testing.T) {
	f := newFixture(t)
	f.users.failErr = errors.New("db connection refused")
	_, err := f.svc.Create("normal_delivery", 3, nil, "admin")
	if err == nil {
		t.Fatal("用户查询故障应返回错误")
	}
	if errors.Is(err, ErrUserNotFound) {
		t.Fatal("DB 故障被吞为 ErrUserNotFound——应返回可区分的包装错误")
	}
	if !strings.Contains(err.Error(), "db connection refused") {
		t.Fatalf("错误链应保留底层原因，实际 %v", err)
	}
}
