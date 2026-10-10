package service

// TP20-T2 用例⑤：切换/重入首回合 prompt 的
// 上下文重建契约——golden 段头+消融（冷启动无前情段）+静态接线守卫（funcSeg 手法
// 同 R115/R129：golden 锁不住"整段接线被摘"的回归面）。
// 依据: 主仓库 .agent/plans/agent-switch-closure-plan-2026-10.md §2.4 用例⑤
// （baton brief golden 同型——R122 graph-reseed TestBuildGraphNodePrompt_SeedSection）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden：非空 brief → 段头固定文案+brief 全文+原 assignment 在后；空 → 原样（消融）。
func TestAssignmentWithBrief_GoldenAndAblation(t *testing.T) {
	const orig = "提交审计任务（原始指派）"
	out := assignmentWithBrief(orig, "\n第3回合：已定位 app.py SQL 注入\n")
	want := "## 前情简报（接续——前序回合档案切片，先核对再行动）\n" +
		"第3回合：已定位 app.py SQL 注入\n\n" + orig
	if out != want {
		t.Fatalf("接续段 golden 失配:\n got: %q\nwant: %q", out, want)
	}
	for _, empty := range []string{"", "   ", "\n\t"} {
		if got := assignmentWithBrief(orig, empty); got != orig {
			t.Fatalf("冷启动消融失守（brief=%q 应原样返回）: %q", empty, got)
		}
	}
}

// 行为级（stub baton 同 writeSSE 夹具族）：brief 非空经真实获取器进 assignment；
// baton 失败=无段（⚑2 降级）。
func TestSeedBriefFn_FeedsAssignmentSection(t *testing.T) {
	dir := t.TempDir()
	// 本地 brief 桩（stubBaton 无 stdout，勿动共享夹具）：brief 子命令回显前情文本
	bin := filepath.Join(dir, "baton-brief-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in brief) echo '第3回合：已定位 app.py SQL 注入';; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", dir)
	// 存档在场（有前情）：brief 输出非空 → 段进 prompt
	writeSSE(t, dir, "T-brief1.sse.log")
	fn := seedBriefFn("T-brief1")
	if fn == nil {
		t.Fatal("BATON_BIN+tasklog 已配置——获取器不应为 nil（禁用档误判）")
	}
	if out := assignmentWithBrief("orig", fn()); !strings.Contains(out, "## 前情简报") {
		t.Fatalf("真实获取器产物未成段: %q", out)
	}
}

// 静态接线守卫：analyzeViaSandbox 必须消费 assignmentWithBrief（摘除接线=M112 牙）。
func TestAnalyzeViaSandbox_BriefWiring(t *testing.T) {
	src, err := os.ReadFile("sandbox_analysis.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func analyzeViaSandbox")
	end := strings.Index(body[start:], "\nfunc ")
	seg := body[start : start+end]
	for _, want := range []string{
		"assignmentWithBrief(assignment, briefFn())",
		"seedBriefFn(taskID)",
	} {
		if !strings.Contains(seg, want) {
			t.Fatalf("analyzeViaSandbox 缺接线 %q（接续简报段整链摘除=上下文重建契约失守）", want)
		}
	}
}

// B12：4a/4b 首回合简报前缀——三态（有档成段/空档原样/禁用档原样/空清单安全）。
func TestPrefaceFirstPrompt(t *testing.T) {
	// 禁用档（BATON_BIN 未设）：原样
	t.Setenv("BATON_BIN", "")
	got := prefaceFirstPrompt([]string{"p0", "p1"}, "T-nop")
	if got[0] != "p0" || got[1] != "p1" {
		t.Fatalf("禁用档应原样: %v", got)
	}
	if prefaceFirstPrompt(nil, "T-nop") != nil {
		t.Fatal("空清单应原样返回 nil")
	}
	// 有档：首回合成段，其余回合不动
	dir := t.TempDir()
	bin := filepath.Join(dir, "baton-brief-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in brief) echo '前情：第2回合结论';; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", dir)
	writeSSE(t, dir, "T-pf.sse.log")
	got = prefaceFirstPrompt([]string{"p0", "p1"}, "T-pf")
	if !strings.Contains(got[0], "## 前情简报") || !strings.Contains(got[0], "前情：第2回合结论") || !strings.HasSuffix(got[0], "p0") {
		t.Fatalf("首回合未成段: %q", got[0])
	}
	if got[1] != "p1" {
		t.Fatalf("非首回合被改动: %q", got[1])
	}
}

// B13：启动降级自检——缺 BATON_BIN/tasklog 时必须产出告警（部署面可见性），
// 配齐=零告警（正常档不打扰）。
func TestStartupDegradationNotices(t *testing.T) {
	t.Setenv("BATON_BIN", "")
	// tasklogDir 有 cfg 缺省档（codeauditcfg）——缺 env 不必然缺配置；
	// 本用例锁 BATON_BIN 面：缺二进制必告警且文案点名接续简报。
	got := StartupDegradationNotices()
	if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "接续简报") {
		t.Fatalf("缺 BATON_BIN 应告警接续简报降级, got %v", got)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "baton-x")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", dir)
	if got := StartupDegradationNotices(); len(got) != 0 {
		t.Fatalf("配齐应零告警, got %v", got)
	}
}
