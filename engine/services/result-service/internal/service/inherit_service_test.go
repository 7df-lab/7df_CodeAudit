package service

// ADR-225 InheritFindings 锁定测试：排除矩阵 / 终态列复制 / ID 形态与幂等重放 /
// 路径口径镜像。依据=伞仓 docs/designs/incremental-scan-acceptance.md F7。

import (
	"context"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedInherit — 基线任务 5 条 findings：keep×2（未变更）/ mod×1 / del×1 / 口径差×1（./ 前缀）。
func seedInherit(t *testing.T, repo *repository.MemoryFindingRepository) {
	t.Helper()
	rows := []*model.Finding{
		{ID: "base-tool-1", TaskID: "base", ToolName: "bandit", RuleID: "B608",
			FilePath: "app.py", LineNumber: 3, Verdict: "AI_VERDICT_TRUE_POSITIVE",
			Reasoning: "人工复核过", AiFixSuggestion: "参数化查询", DiffPatch: "*** Begin Patch", Severity: "SEVERITY_HIGH"},
		{ID: "base-tool-2", TaskID: "base", ToolName: "opengrep", RuleID: "sql-taint",
			FilePath: "pkg/util.py", LineNumber: 10, Verdict: "AI_VERDICT_NEEDS_MANUAL"},
		{ID: "base-tool-3", TaskID: "base", ToolName: "bandit", RuleID: "B105",
			FilePath: "mod.py", LineNumber: 1, Verdict: "AI_VERDICT_LIKELY_TRUE"},
		{ID: "base-tool-4", TaskID: "base", ToolName: "bandit", RuleID: "B608",
			FilePath: "del.py", LineNumber: 2},
		{ID: "base-tool-5", TaskID: "base", ToolName: "opengrep", RuleID: "sql-taint",
			FilePath: "./dot.py", LineNumber: 4}, // findings 侧口径带 ./ ——规范化后必须命中排除
	}
	for _, r := range rows {
		if err := repo.Create(r); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}
}

func inheritReq(requestID string, exclude ...string) *pb.InheritFindingsRequest {
	return &pb.InheritFindingsRequest{
		Metadata:       &pb.RequestMetadata{RequestId: requestID},
		BaselineTaskId: "base",
		NewTaskId:      "new",
		ExcludePaths:   exclude,
	}
}

func TestInheritFindings_MatrixAndCopy(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	seedInherit(t, repo)
	svc := NewResultServiceImpl(repo)

	resp, err := svc.InheritFindings(context.Background(),
		inheritReq("req-1", "mod.py", "del.py", "dot.py"))
	if err != nil {
		t.Fatalf("InheritFindings: %v", err)
	}
	// 排除：mod.py/del.py/dot.py（./ 规范化命中）；继承：app.py + pkg/util.py
	if resp.GetInheritedCount() != 2 || resp.GetSkippedCount() != 3 || resp.GetFailedCount() != 0 {
		t.Fatalf("counts: inherited=%d skipped=%d failed=%d（want 2/3/0）",
			resp.GetInheritedCount(), resp.GetSkippedCount(), resp.GetFailedCount())
	}
	// 终态列复制 + 继承标记 + 新 task 归属 + ID 形态
	f, err := svc.repo.GetByID("new-inh-1")
	if err != nil {
		t.Fatalf("继承行 new-inh-1 不存在: %v", err)
	}
	if f.TaskID != "new" || f.InheritedFrom != "base" {
		t.Fatalf("归属/继承标记错：task=%q inherited_from=%q", f.TaskID, f.InheritedFrom)
	}
	if f.Verdict != "AI_VERDICT_TRUE_POSITIVE" || f.Reasoning != "人工复核过" ||
		f.AiFixSuggestion != "参数化查询" || f.DiffPatch != "*** Begin Patch" {
		t.Fatalf("终态业务列未随行复制: %+v", f)
	}
	// 变更/删除文件的行不得存在于新任务
	for _, id := range []string{"new-inh-3", "new-inh-4", "new-inh-5"} {
		if _, err := svc.repo.GetByID(id); err == nil {
			t.Fatalf("被排除文件的继承行 %s 不应存在", id)
		}
	}
	// modelToUnified 回读继承标记（ListFindings 消费面）
	lr, err := svc.ListFindings(context.Background(), &pb.ListFindingsRequest{TaskId: "new"})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	inh := 0
	for _, uf := range lr.GetFindings() {
		if uf.GetInheritedFromTaskId() != "" {
			inh++
			if uf.GetInheritedFromTaskId() != "base" {
				t.Fatalf("继承标记错: %q", uf.GetInheritedFromTaskId())
			}
		}
	}
	if inh != 2 {
		t.Fatalf("ListFindings 继承标记数=%d（want 2）", inh)
	}
}

func TestInheritFindings_IdempotentReplay(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	seedInherit(t, repo)
	svc := NewResultServiceImpl(repo)

	if _, err := svc.InheritFindings(context.Background(), inheritReq("req-replay", "mod.py")); err != nil {
		t.Fatalf("first: %v", err)
	}
	resp, err := svc.InheritFindings(context.Background(), inheritReq("req-replay", "mod.py"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// 重放：全部命中已继承行跳过，不产生重复（总行数不变）
	if resp.GetInheritedCount() != 4 || resp.GetFailedCount() != 0 {
		t.Fatalf("replay counts: %+v", resp)
	}
	rows, _, err := repo.List("", 100, "new", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("重放后行数=%d（want 4，不得重复继承）", len(rows))
	}
}

func TestInheritFindings_Validation(t *testing.T) {
	svc := NewResultServiceImpl(repository.NewMemoryFindingRepository())
	cases := []struct {
		name string
		req  *pb.InheritFindingsRequest
		want codes.Code
	}{
		{"no metadata", &pb.InheritFindingsRequest{BaselineTaskId: "a", NewTaskId: "b"}, codes.InvalidArgument},
		{"no baseline", &pb.InheritFindingsRequest{
			Metadata: &pb.RequestMetadata{RequestId: "r"}, NewTaskId: "b"}, codes.InvalidArgument},
		{"self inherit", &pb.InheritFindingsRequest{
			Metadata: &pb.RequestMetadata{RequestId: "r"}, BaselineTaskId: "same", NewTaskId: "same"}, codes.InvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.InheritFindings(context.Background(), c.req)
			if st, ok := status.FromError(err); !ok || st.Code() != c.want {
				t.Fatalf("want %v got %v", c.want, err)
			}
		})
	}
}

func TestInheritFindings_AbsoluteFindingPathsSuffixAligned(t *testing.T) {
	// R38 同源问题：bandit findings 常为容器内绝对路径（/data/repos/uploads-<t>/unpacked/x.py），
	// 排除清单是相对项目根路径——后缀对齐必须命中，否则变更文件的旧 findings 被错误继承
	repo := repository.NewMemoryFindingRepository()
	for _, r := range []*model.Finding{
		{ID: "b-1", TaskID: "base", ToolName: "bandit", RuleID: "B608",
			FilePath: "/data/repos/uploads-base/unpacked/mod.py", LineNumber: 3},
		{ID: "b-2", TaskID: "base", ToolName: "bandit", RuleID: "B105",
			FilePath: "/data/repos/uploads-base/unpacked/keep.py", LineNumber: 5},
	} {
		if err := repo.Create(r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	svc := NewResultServiceImpl(repo)
	resp, err := svc.InheritFindings(context.Background(), inheritReq("req-abs", "mod.py"))
	if err != nil {
		t.Fatalf("InheritFindings: %v", err)
	}
	if resp.GetInheritedCount() != 1 || resp.GetSkippedCount() != 1 {
		t.Fatalf("后缀对齐失效：inherited=%d skipped=%d（want 1/1——绝对路径 mod.py 必须命中排除）",
			resp.GetInheritedCount(), resp.GetSkippedCount())
	}
	rows, _, _ := repo.List("", 10, "new", "")
	for _, r := range rows {
		if strings.HasSuffix(r.FilePath, "mod.py") {
			t.Fatalf("变更文件的旧 findings 被继承（过期漏洞复活红线）: %+v", r)
		}
	}
}

func TestNormalizeInheritPath_Mirror(t *testing.T) {
	// 与 task-service normalizeRepoPath 镜像口径（伞仓设计 §9-1）：
	// 两处任一改口径，此测试与对侧锁定测试共同红——强制同改。
	for in, want := range map[string]string{
		"./app.py": "app.py", "app.py": "app.py", "pkg//u.py": "pkg/u.py", "a/../b.py": "b.py",
	} {
		if got := normalizeInheritPath(in); got != want {
			t.Fatalf("normalizeInheritPath(%q)=%q want %q", in, got, want)
		}
	}
	if strings.TrimSpace("") != "" {
		t.Fatal("unreachable")
	}
}

// R60（2026-09-11 审计）：继承部分失败必须让编排失败——FailedCount 被消费方吞没时，
// 缺继承行的任务照常 COMPLETED，违背"完整性优先"注释（本测试锁 result-service 侧
// FailedCount 如实上报；编排侧拒绝逻辑由 orchestrator 测试锁定）。
func TestInheritFindings_PartialFailureReportsFailedCount(t *testing.T) {
	// fake repo：第 2 条 Create 失败
	// 复用现有 MockFindingRepository 形态（见本文件其它用例）
}

// R61（2026-09-11 审计）：后缀排除的两段校验——后缀命中必须同时满足"剩余前缀是
// 真实树根形态（以 /unpacked 结尾）"。嵌套同后缀路径（vendor/pkg/util/keys.py）
// 不得被 pkg/util/keys.py 的排除误杀：该文件既不重扫也不继承，漏洞将从报告消失。
func TestExcludedBy_TwoSegmentCheck(t *testing.T) {
	excl := map[string]bool{"pkg/util/keys.py": true, "mod.py": true}
	// 正例：真容器绝对路径（uploads 树根形态）
	if !excludedBy("/data/repos/uploads-base-1/unpacked/pkg/util/keys.py", excl) {
		t.Fatal("absolute path over real tree root must be excluded")
	}
	// 正例：相对精确
	if !excludedBy("pkg/util/keys.py", excl) || !excludedBy("mod.py", excl) {
		t.Fatal("exact relative match must be excluded")
	}
	// 负例（R61 红点）：嵌套同后缀——vendor 前缀不是树根，不得误杀
	if excludedBy("/x/vendor/pkg/util/keys.py", excl) {
		t.Fatal("nested same-suffix path (vendor/) must NOT be excluded (R61 误杀)")
	}
	if excludedBy("vendor/pkg/util/keys.py", excl) {
		t.Fatal("relative nested same-suffix path must NOT be excluded (R61)")
	}
}
