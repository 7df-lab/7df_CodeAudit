package service

// ADR-241第二阶段编排回归：
//   - applyPatchPhase 清单构建（index/title/file_path/start_line/fix_suggestion，不带
//     reasoning）与按 index 合并（好补丁规范化落位 / 显式空保持空 / 坏补丁 raw 留三阶段 /
//     越界丢弃）；
//   - 三阶段衔接锁：坏补丁 raw 非空 → retryFailedPatches 触发条件"非空且校验失败"自然
//     接管，自纠产出经复验落位——两段管线无缝衔接、fixretry 触发条件不变；
//   - 诚实降级：补丁回合失败 findings 原样返回（不静默伪装）；
//   - 零发现零开销（不发回合）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// phase1Findings — 一阶段（纯检测）产出的 findings 形状：diff_patch 恒空。
func phase1Findings() []sandbox.Finding {
	return []sandbox.Finding{
		{Title: "SQL 注入", FilePath: "vuln_fix_demo.py", StartLine: 6,
			FixSuggestion: "改参数化查询", Reasoning: "判定论证留在检测阶段"},
		{Title: "无把握项", FilePath: "vuln_fix_demo.py", StartLine: 9,
			FixSuggestion: ""},
		{Title: "XSS", FilePath: "other.py", StartLine: 1,
			FixSuggestion: "转义输出"},
	}
}

func TestApplyPatchPhase_MergesByIndex(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	var gotAssignment string
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		gotAssignment = a
		return []sandbox.PatchFix{
			{Index: 0, DiffPatch: vulnFixDemoPatch},  // 好补丁 → 规范化落位
			{Index: 1, DiffPatch: ""},                // 显式空 → 保持空（无把握不代写）
			{Index: 2, DiffPatch: vulnFixDemoPatch},  // 补丁与 other.py 无关也照落（校验只对工作区文件）
			{Index: 99, DiffPatch: vulnFixDemoPatch}, // 越界 → 丢弃
		}, nil
	}
	findings := applyPatchPhase(context.Background(), dir, phase1Findings(), nil, round)

	// 清单含位置与意图字段、不含 reasoning 全文
	for _, want := range []string{`"index":0`, `"title":"SQL 注入"`, `"file_path":"vuln_fix_demo.py"`,
		`"start_line":6`, `"fix_suggestion":"改参数化查询"`, sandbox.ProjectSandboxPath} {
		if !strings.Contains(gotAssignment, want) {
			t.Fatalf("phase-2 assignment missing %q:\n%.400s", want, gotAssignment)
		}
	}
	if strings.Contains(gotAssignment, "判定论证留在检测阶段") {
		t.Fatal("phase-2 manifest must not carry reasoning")
	}
	if !strings.HasPrefix(findings[0].DiffPatch, "*** Begin Patch") {
		t.Fatalf("valid patch must be normalized in place:\n%s", findings[0].DiffPatch)
	}
	if findings[1].DiffPatch != "" {
		t.Fatalf("explicit empty must stay empty, got %q", findings[1].DiffPatch)
	}
	if findings[2].DiffPatch == "" {
		t.Fatal("patch for index 2 must land")
	}
}

func TestApplyPatchPhase_BadPatchStaysForPhase3(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	badPatch := strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( bad )", 1)
	phase2Called, phase3Called := false, false
	phase2 := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		phase2Called = true
		return []sandbox.PatchFix{{Index: 0, DiffPatch: badPatch}}, nil // 校验失败 → raw 保留
	}
	findings := applyPatchPhase(context.Background(), dir, phase1Findings()[:1], nil, phase2)
	if !phase2Called {
		t.Fatal("phase-2 round must run for non-empty findings")
	}
	if findings[0].DiffPatch != badPatch {
		t.Fatalf("invalid patch must stay raw for phase-3 pickup, got:\n%s", findings[0].DiffPatch)
	}
	// 三阶段衔接：fixretry 触发条件（非空且校验失败）自然接住，自纠产出复验落位
	phase3 := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		phase3Called = true
		if !strings.Contains(a, "validation_error") {
			t.Fatalf("phase-3 feedback assignment must carry failure detail:\n%.300s", a)
		}
		return []sandbox.PatchFix{{Index: 0, DiffPatch: vulnFixDemoPatch}}, nil
	}
	final := retryFailedPatches(context.Background(), "t-2phase", dir, findings, nil, phase3)
	if !phase3Called {
		t.Fatal("phase-3 fixretry must trigger on bad patch from phase-2")
	}
	if !strings.HasPrefix(final[0].DiffPatch, "*** Begin Patch") || final[0].DiffPatch == badPatch {
		t.Fatalf("phase-3 must replace with normalized patch:\n%s", final[0].DiffPatch)
	}
}

func TestApplyPatchPhase_RoundFailureHonestDowngrade(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	var logs []string
	emit := func(level, msg string) { logs = append(logs, level+": "+msg) }
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		return nil, fmt.Errorf("sandbox down")
	}
	findings := applyPatchPhase(context.Background(), dir, phase1Findings(), emit, round)
	for i := range findings {
		if findings[i].DiffPatch != "" {
			t.Fatalf("round failure must keep findings patch-free, index %d = %q", i, findings[i].DiffPatch)
		}
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "warn") || !strings.Contains(joined, "补丁生成回合失败") {
		t.Fatalf("downgrade must be visible (D1 诚实降级), logs:\n%s", joined)
	}
}

func TestApplyPatchPhase_NoFindingsNoRound(t *testing.T) {
	called := false
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		called = true
		return nil, nil
	}
	got := applyPatchPhase(context.Background(), "unused", nil, nil, round)
	if called {
		t.Fatal("zero findings → no phase-2 round (零开销)")
	}
	if len(got) != 0 {
		t.Fatal("empty in empty out")
	}
}
