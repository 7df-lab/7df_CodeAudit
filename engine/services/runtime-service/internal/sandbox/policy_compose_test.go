package sandbox

// ADR-248 切片4后半锁定测试：targeted spec.policy 组合器——§I8 契约面
// （整档携带/wire-name 特例/endpoints 唯一写入点/单向收紧/fail-closed/digest 锚）。
// 模板=仓内真件（configs/policy/targeted.yaml，随 clone 存在）——测试锁的就是
// 部署树将消费的那份文件。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

const repoTemplatePath = "../../../../configs/policy/targeted.yaml"

func TestComposeTargetedPolicy(t *testing.T) {
	targets := []targetEndpoint{
		{Host: "target-a.example", Port: 443, AllowedIPs: []string{"203.0.113.7", "198.51.100.0/24"}},
		{Host: "target-b.example", Port: 8080, AllowedIPs: []string{"192.0.2.1"}},
	}
	policy, digest, err := ComposeTargetedPolicy(repoTemplatePath, targets)
	if err != nil {
		t.Fatal(err)
	}
	// wire-name 特例（§I8 网关 400 枚举实证）
	if _, ok := policy["filesystem"]; !ok {
		t.Fatalf("filesystem_policy 必须映射为 filesystem（wire-name 特例）: keys=%v", policyKeys(policy))
	}
	if _, ok := policy["filesystem_policy"]; ok {
		t.Fatal("filesystem_policy 原名不得残留")
	}
	// 整档携带：landlock/process 段保留
	if policy["landlock"] == nil || policy["process"] == nil {
		t.Fatalf("整档携带破坏（landlock/process 缺失）: %v", policyKeys(policy))
	}
	nps := policy["network_policies"].(map[string]any)
	base := nps["codeaudit_inference"].(map[string]any)
	if base["name"] != "codeaudit-inference" {
		t.Fatalf("基础档被触碰（B1：组合器不得改模板其余内容）: %v", base)
	}
	env := nps["authorized_targets"].(map[string]any)
	eps := env["endpoints"].([]any)
	if len(eps) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(eps))
	}
	e0 := eps[0].(map[string]any)
	if e0["host"] != "target-a.example" || e0["port"].(uint32) != 443 || e0["protocol"] != "tcp" {
		t.Fatalf("端点形状（§I8 激活配方）: %v", e0)
	}
	if ips, ok := e0["allowed_ips"].([]string); !ok || len(ips) != 2 || ips[1] != "198.51.100.0/24" {
		t.Fatalf("allowed_ips 钉扎: %v", e0["allowed_ips"])
	}
	// digest=组合时模板内容锚
	raw, _ := os.ReadFile(repoTemplatePath)
	sum := sha256.Sum256(raw)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest 与模板文件不符（C3 锚）")
	}
}

func TestComposeTargetedPolicy_EmptyTargets_FailClosed(t *testing.T) {
	policy, _, err := ComposeTargetedPolicy(repoTemplatePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := policy["network_policies"].(map[string]any)["authorized_targets"].(map[string]any)
	eps := env["endpoints"].([]any)
	if len(eps) != 0 {
		t.Fatalf("烘焙态 endpoints 须恒空（fail-closed），got %v", eps)
	}
}

func TestComposeTargetedPolicy_TemplateMissing_FailClosed(t *testing.T) {
	_, _, err := ComposeTargetedPolicy(filepath.Join(t.TempDir(), "absent.yaml"), nil)
	if err == nil {
		t.Fatal("模板缺失必须 fail-closed 报错，不静默回落")
	}
}

func TestComposeTargetedPolicy_BadTemplateShape(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.yaml")
	// 缺 authorized_targets 规则的假模板
	if err := os.WriteFile(p, []byte("version: 1\nnetwork_policies:\n  codeaudit_inference:\n    name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ComposeTargetedPolicy(p, nil); err == nil {
		t.Fatal("形状断言失败必须报错（两规则须在）")
	}
}

// TestSandboxSpec_RestrictedNoPolicy — B1：restricted（含空 profile）永不携带
// spec.policy（缺省权威=镜像烘焙件，设计 §2）；targeted 携带+digest 审计位。
func TestSandboxSpec_RestrictedNoPolicy(t *testing.T) {
	spec, err := sandboxSpecWithPolicy("ca-x", "t-1", Config{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec["policy"]; ok {
		t.Fatal("restricted 永不携带 spec.policy（B1——镜像烘焙件唯一权威）")
	}
	specR, err := sandboxSpecWithPolicy("ca-x", "t-1", Config{Image: "img", NetworkProfile: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := specR["policy"]; ok {
		t.Fatal("profile=restricted 同样不携带")
	}
	specT, err := sandboxSpecWithPolicy("ca-x", "t-1", Config{
		Image: "img", NetworkProfile: "targeted", PolicyTemplate: repoTemplatePath,
		AuthorizedTargets: []targetEndpoint{{Host: "t.example", Port: 443, AllowedIPs: []string{"203.0.113.7"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if specT["policy"] == nil || specT["_policy_digest"] == "" {
		t.Fatal("targeted 须携带 policy+digest 审计位")
	}
}

// TestSandboxSpec_TargetedComposeFailure_FailClosed — 组合失败=error（launch
// 失败可见），不静默产出无 policy 的 restricted 形状（D1）。
func TestSandboxSpec_TargetedComposeFailure_FailClosed(t *testing.T) {
	_, err := sandboxSpecWithPolicy("ca-x", "t-1", Config{
		Image: "img", NetworkProfile: "targeted", PolicyTemplate: "/nonexistent/t.yaml",
	})
	if err == nil {
		t.Fatal("targeted 模板缺失必须报错（fail-closed），不得静默回落 restricted")
	}
}

func policyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
