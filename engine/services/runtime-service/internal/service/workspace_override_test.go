package service

// ADR-247（切片3后半）锁定测试：TaskOrigin.workspace → sandbox.Config 覆盖——
// per-team 命名空间贯穿的单点执法面（沙箱归属+manager inference 寻址同源
// cfg.Workspace）。team 隔离行为锁在 cfg 层（纯函数）；三入口+admin 面接线用
// 结构性守卫（pg_bounded 同型）；端到端寻址归 sim。

import (
	"os"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// TestApplyWorkspaceOverride — 覆盖三态 + team 隔离（两 team 互不串区）。
func TestApplyWorkspaceOverride(t *testing.T) {
	// 非空覆盖全局缺省
	cfgA := &sandbox.Config{Workspace: "default"}
	applyWorkspaceOverride(cfgA, &pb.TaskOrigin{Workspace: "team-a"})
	if cfgA.Workspace != "team-a" {
		t.Fatalf("override: got %q", cfgA.Workspace)
	}
	// team 隔离：同缺省出发，两 team 各自寻址
	cfgB := &sandbox.Config{Workspace: "default"}
	applyWorkspaceOverride(cfgB, &pb.TaskOrigin{Workspace: "team-b"})
	if cfgA.Workspace == cfgB.Workspace || cfgB.Workspace != "team-b" {
		t.Fatalf("team isolation broken: A=%q B=%q", cfgA.Workspace, cfgB.Workspace)
	}
	// 空串/nil origin=回落全局缺省（既有部署零改动）
	cfgC := &sandbox.Config{Workspace: "default"}
	applyWorkspaceOverride(cfgC, &pb.TaskOrigin{Workspace: ""})
	applyWorkspaceOverride(cfgC, nil)
	if cfgC.Workspace != "default" {
		t.Fatalf("fallback must stay default: got %q", cfgC.Workspace)
	}
}

// TestApplyOverride_NetworkFields — ADR-248：profile/targets 随 origin 贯穿
// （nil origin/无字段=restricted 缺省态零改动）。
func TestApplyOverride_NetworkFields(t *testing.T) {
	cfg := &sandbox.Config{Workspace: "default"}
	applyWorkspaceOverride(cfg, &pb.TaskOrigin{
		NetworkProfile: "targeted",
		AuthorizedTargets: []*pb.AuthorizedTarget{
			{Host: "t.example", Port: 443, AllowedIps: []string{"203.0.113.7"}},
		},
	})
	if cfg.NetworkProfile != "targeted" || len(cfg.AuthorizedTargets) != 1 || cfg.AuthorizedTargets[0].Host != "t.example" {
		t.Fatalf("network fields = %+v", cfg)
	}
	plain := &sandbox.Config{}
	applyWorkspaceOverride(plain, nil)
	if plain.NetworkProfile != "" || len(plain.AuthorizedTargets) != 0 {
		t.Fatalf("nil origin=restricted 缺省态: %+v", plain)
	}
}

// TestWorkspaceOverride_WiredAllEntryPoints — 结构性守卫：三沙箱入口+admin 装配
// 均接覆盖点；inferenceRunner 收 workspace 参数。任一入口漏接即 per-team 断链。
func TestWorkspaceOverride_WiredAllEntryPoints(t *testing.T) {
	for _, c := range []struct{ file, anchor string }{
		{"sandbox_analysis.go", "applyWorkspaceOverride(cfg, origin) // ADR-247: per-team workspace 贯穿"},
		{"sandbox_verify.go", "applyWorkspaceOverride(cfg, origin) // ADR-247: per-team workspace 贯穿"},
		{"inference_admin.go", "func inferenceRunner(workspace string) (*sandbox.ManagerRunner, error) {"},
	} {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if !strings.Contains(string(raw), c.anchor) {
			t.Fatalf("ADR-247 wiring missing in %s: %q", c.file, c.anchor)
		}
	}
	// admin 6 RPC 全数传请求级 workspace（防"部分面漏接"回潮）
	raw, err := os.ReadFile("inference_admin.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "inferenceRunner(req.GetWorkspace())"); n != 6 {
		t.Fatalf("admin RPCs wiring: want 6 req.GetWorkspace(), got %d", n)
	}
}
