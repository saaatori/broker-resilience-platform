package task

import (
	"bytes"
	"os"
	"testing"
)

const labDefaultParamsPath = "../../../lab/scenarios/normal_delivery/params.default.json"

// TestSeedScenarioInsertsDefault 首次 seed 插入 normal_delivery，default_params=embed 副本原文。
func TestSeedScenarioInsertsDefault(t *testing.T) {
	repo := newFakeTaskRepo()
	svc := newTestService(t, repo, nil, nil)
	if err := svc.SeedScenario(); err != nil {
		t.Fatalf("SeedScenario: %v", err)
	}
	s, ok := repo.scenarios["normal_delivery"]
	if !ok {
		t.Fatal("seed 后 test_scenario 应有 normal_delivery 行")
	}
	want, err := seedFS.ReadFile("seed/normal_delivery.json")
	if err != nil {
		t.Fatalf("读 embed 副本: %v", err)
	}
	if s.DefaultParams != string(want) {
		t.Fatalf("default_params 应为副本原文:\n got: %s\nwant: %s", s.DefaultParams, want)
	}
	if !s.Enabled || s.Name == "" {
		t.Fatalf("场景应启用且有名称: %+v", s)
	}
}

// TestSeedScenarioIdempotent 二次 seed 幂等：仍一行、无错（服务重启路径）。
func TestSeedScenarioIdempotent(t *testing.T) {
	repo := newFakeTaskRepo()
	svc := newTestService(t, repo, nil, nil)
	if err := svc.SeedScenario(); err != nil {
		t.Fatalf("首次 SeedScenario: %v", err)
	}
	if err := svc.SeedScenario(); err != nil {
		t.Fatalf("二次 SeedScenario 应幂等: %v", err)
	}
	if len(repo.scenarios) != 1 {
		t.Fatalf("二次 seed 后应仍只有一行，实际 %d", len(repo.scenarios))
	}
}

// TestSeedCopyMatchesLab 防漂移钉子：seed 副本与 lab 场景默认参数逐字节相等——
// lab 侧定稿值变更（day26 纪律：快照默认值改动走 params.default.json）而 Go 副本未跟随时变红。
func TestSeedCopyMatchesLab(t *testing.T) {
	embedRaw, err := seedFS.ReadFile("seed/normal_delivery.json")
	if err != nil {
		t.Fatalf("读 embed 副本: %v", err)
	}
	labRaw, err := os.ReadFile(labDefaultParamsPath)
	if err != nil {
		t.Fatalf("读 lab 默认参数: %v", err)
	}
	if !bytes.Equal(embedRaw, labRaw) {
		t.Fatal("embed seed 副本与 lab/scenarios/normal_delivery/params.default.json 不一致——请同步副本")
	}
}
