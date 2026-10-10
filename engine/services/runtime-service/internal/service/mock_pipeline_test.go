package service

// mock 条目 × 两段式管线集成（R6 v0，切片 1 DoD"加 descriptor 条目零代码改动拉起新
// agent（用 mock 条目验收即可闭环）"）：一阶段 findings → applyPatchPhase 二阶段
//（真 NormalizeDiffPatch 对工作区锚定校验）→ 三阶段衔接面，全部由纯条目数据驱动。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §4.1/§5；ADR-244。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// mockProject — 工作区夹具：src/mock.go 逐字含 mock 补丁生成器的锚定/上下文/删除行
// （NormalizeDiffPatch fuzz=0 锚定校验的消费端事实源）。
func mockProject(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("package mock\n\n")
	for i := 0; i < n; i++ {
		b.WriteString("// mock anchor " + strconv.Itoa(i) + "\ncontext line " + strconv.Itoa(i) + "\nremoved line " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "mock.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyPatchPhase_MockAdapterFullPipeline(t *testing.T) {
	t.Setenv("CODEAUDIT_MOCK_DIALECT_ALLOW", "1")
	t.Setenv("CODEAUDIT_MOCK_SEED", "42")
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "structured")
	t.Setenv("CODEAUDIT_MOCK_FINDINGS", "4")
	t.Setenv("CODEAUDIT_MOCK_DELAY_MS", "")

	project := mockProject(t, 4)
	d := sandbox.DefaultDescriptor()
	d.ID = "mock"
	d.Dialect = "mock-stdio"
	d.Session = sandbox.SessionSpec{Transport: "mock-inproc", IdleConvergence: "turn-based", Resume: "none"}
	d.Healthcheck = sandbox.HealthSpec{Mode: "none"}
	r := sandbox.NewManagerRunner(sandbox.Config{Mode: "openshell", Agent: d})

	// 一阶段：mock 回合产出 findings（Run 编排经方言注册表分派，无 manager 交互）。
	res, err := r.Run(context.Background(), sandbox.Task{TaskID: "gw-mockpipe", WorkspaceDir: project, Assignment: "审计它"})
	if err != nil || len(res.Findings) != 4 {
		t.Fatalf("phase-1 failed: %v findings=%d", err, len(res.Findings))
	}

	// 二阶段：真 applyPatchPhase，patchRound 闭包=同一 runner 的 mock 补丁回合
	//（与 analyzeViaSandbox 的 patchRound 同形——服务编排零改动，仅方言不同）。
	var logs []string
	emit := func(level, msg string) { logs = append(logs, level+": "+msg) }
	patchRound := func(ctx context.Context, fixAssignment string) ([]sandbox.PatchFix, error) {
		fixRes, ferr := r.Run(ctx, sandbox.Task{
			TaskID: "gw-mockpipe-fix", WorkspaceDir: project, PatchFixRound: true,
			Assignment: fixAssignment,
		})
		if ferr != nil {
			return nil, ferr
		}
		return fixRes.Patches, nil
	}
	got := applyPatchPhase(context.Background(), project, res.Findings, emit, patchRound)

	merged := 0
	for i := range got {
		f := got[i]
		if f.DiffPatch == "" {
			continue // mock 1/3 概率显式空串（无把握不代写，与三阶段同口径）
		}
		merged++
		if !strings.HasPrefix(f.DiffPatch, "*** Begin Patch") {
			t.Fatalf("finding %d patch not normalized: %.80s", i, f.DiffPatch)
		}
	}
	if merged == 0 {
		t.Fatal("seed 42 下至少应有补丁落位（全空=夹具失效）")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "补丁生成结果：有效补丁") {
		t.Fatalf("applyPatchPhase 结果行缺失: %v", joined)
	}
}
