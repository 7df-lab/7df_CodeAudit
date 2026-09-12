package fusion

// R38 回归（2026-09-09 用户报障"同一文件同一行的漏洞显示成两个风险"）：
// SAST（opengrep）产出的 location.file_path 是沙箱绝对路径，AI（ai_agent）结论引用
// 的是裸文件名/相对路径——MergeStage 此前按 file_path 字符串精确相等建索引匹配，
// 两者永不命中 → 同文件同行漏洞 SAST+AI 各出一条（is_unique 双真）。
// 修复 = 路径后缀对齐匹配：AI 路径（归一化后）是 SAST 路径的尾部（"/"+ai 结尾）或
// 相等即视为同文件；行号仍要求 start_line 相等。负例锁定不误并（异名文件/异行不合并）。

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func mergeSeed(sastPath string, aiPath string, aiLine int32) (*FusionContext, *pb.UnifiedFinding, *pb.UnifiedFinding) {
	sast := &pb.UnifiedFinding{
		FindingId:  "sast-1",
		SourceTool: "opengrep",
		Location:   &pb.LocationInfo{FilePath: sastPath, StartLine: 88, EndLine: 88},
	}
	ai := &pb.UnifiedFinding{
		FindingId:  "ai-1",
		SourceTool: "ai_agent",
		Location:   &pb.LocationInfo{FilePath: aiPath, StartLine: aiLine},
	}
	ctx := &FusionContext{
		FilteredSAST: []*pb.UnifiedFinding{sast},
		FilteredAI:   []*pb.UnifiedFinding{ai},
		Metrics:      &FusionMetrics{},
	}
	return ctx, sast, ai
}

func TestMergeStage_PathSuffixAlignment_MergesSameVuln(t *testing.T) {
	// 沙箱绝对路径 × 裸文件名（用户报障形态）
	cases := []struct {
		name, sastPath, aiPath string
		aiLine                 int32
	}{
		{"bare filename", "/data/repos/gw-x/src/app.py", "app.py", 88},
		{"relative subdir", "/data/repos/gw-x/src/app.py", "src/app.py", 88},
		{"dot-slash prefix", "/data/repos/gw-x/src/app.py", "./app.py", 88},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, sast, ai := mergeSeed(tc.sastPath, tc.aiPath, tc.aiLine)
			out, err := NewMergeStage().Execute(context.Background(), input)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(out.Groups) != 1 {
				t.Fatalf("same-vuln finding must merge into one group, got %d (pre-fix: exact-string match never hits)", len(out.Groups))
			}
			if sast.GetAiVerdict() != pb.AIVerdict_AI_VERDICT_UNSPECIFIED {
				t.Fatalf("unexpected verdict writeback")
			}
			found := false
			for _, id := range sast.GetMatchedFindings() {
				if id == ai.GetFindingId() {
					found = true
				}
			}
			if !found {
				t.Fatalf("matched_findings must record the AI member")
			}
			if sast.GetIsUnique() || ai.GetIsUnique() {
				t.Fatalf("merged findings must not be marked unique")
			}
		})
	}
}

func TestMergeStage_PathSuffixAlignment_NegativeControls(t *testing.T) {
	cases := []struct {
		name, sastPath, aiPath string
		aiLine                 int32
	}{
		{"different file same line", "/data/repos/gw-x/src/app.py", "other.py", 88},
		{"same file different line", "/data/repos/gw-x/src/app.py", "app.py", 12},
		{"partial-name not suffix", "/data/repos/gw-x/src/app.py", "p.py", 88},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, sast, ai := mergeSeed(tc.sastPath, tc.aiPath, tc.aiLine)
			out, err := NewMergeStage().Execute(context.Background(), input)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(out.Groups) != 0 {
				t.Fatalf("distinct locations must not merge, got %d groups", len(out.Groups))
			}
			if !sast.GetIsUnique() || !ai.GetIsUnique() {
				t.Fatalf("non-merged findings must stay unique")
			}
		})
	}
}
