package service

// ADR-183 补遗② 真实链路证据（env 门控，默认跳过——门禁不依赖外部沙箱）：
// CODEAUDIT_LIVE_FIXRETRY=1 时，向真实沙箱（openshell-manager 通道）注入一条
// 上下文被改写的坏补丁，验证"失败反馈再生成"全链：失败详情构建→真 LLM 自纠→
// 严格复验→原位替换。证据归档 .agent/evidence/adr183_diff_patch/。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

func TestLiveRetryFailedPatchesRealSandbox(t *testing.T) {
	if os.Getenv("CODEAUDIT_LIVE_FIXRETRY") != "1" {
		t.Skip("live-only: CODEAUDIT_LIVE_FIXRETRY=1（真实沙箱回合，证据归档用）")
	}
	cfg, err := sandboxCfg()
	if err != nil {
		t.Fatalf("sandbox config: %v", err)
	}
	ws := os.Getenv("CODEAUDIT_LIVE_FIXRETRY_WS")
	if ws == "" {
		t.Fatal("CODEAUDIT_LIVE_FIXRETRY_WS=<工作区目录> required")
	}
	badPatch := strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( query )", 1)
	findings := []sandbox.Finding{{
		Title: "SQL 注入（拼接构造查询）", Severity: "SEVERITY_HIGH", CweID: "CWE-89",
		FilePath: "vuln_fix_demo.py", StartLine: 6, Confidence: 0.95,
		Reasoning: "字符串拼接进 SQL", FixSuggestion: "改参数化查询",
		DiffPatch: badPatch,
	}}
	r := sandbox.NewManagerRunner(*cfg)
	round := func(ctx context.Context, assignment string) (string, error) {
		res, rerr := r.Run(ctx, sandbox.Task{
			TaskID: "live-fixretry", WorkspaceDir: ws, Assignment: assignment,
			Timeout: 10 * time.Minute, // 07 §8
		})
		if rerr != nil {
			return "", rerr
		}
		return res.FinalText, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	got := retryFailedPatches(ctx, "live-fixretry", ws, findings, func(level, msg string) {
		t.Logf("[tasklog:%s] %s", level, msg)
	}, round)
	dp := got[0].DiffPatch
	t.Logf("再生成后 diff_patch:\n%s", dp)
	if dp == badPatch {
		t.Fatal("real retry did not replace the bad patch (still raw)")
	}
	normalized, verr := NormalizeDiffPatch(dp, ws)
	if verr != nil {
		t.Fatalf("regenerated patch must pass strict gate: %v", verr)
	}
	t.Logf("复验通过（规范化输出):\n%s", normalized)
}
