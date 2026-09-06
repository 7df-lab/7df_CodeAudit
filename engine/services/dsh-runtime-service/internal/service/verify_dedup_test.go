// verify_dedup 单测 — 同文件同段去重（ADR-186）：分组边界、代表选择、判定广播。
package service

import (
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func mkFinding(id, file string, start, end int32, sev pb.Severity, tool, rule string) *pb.UnifiedFinding {
	return &pb.UnifiedFinding{
		FindingId:   id,
		SourceTool:  tool,
		SourceRuleId: rule,
		Severity:    sev,
		Location:    &pb.LocationInfo{FilePath: file, StartLine: start, EndLine: end},
	}
}

// 同文件行漂移 ±2 行（容差半径内）→ 归并为一组
func TestGroupSegments_MergesLineDriftWithinRadius(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "a.py", 42, 42, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "a.py", 44, 44, pb.Severity_SEVERITY_HIGH, "bandit", "B608"),
	}
	gs := groupSegments(fs)
	if len(gs) != 1 {
		t.Fatalf("want 1 group, got %d", len(gs))
	}
	if len(gs[0].Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(gs[0].Members))
	}
}

// 恰在容差边界（距离 = 半径+1）→ 不归并
func TestGroupSegments_NoMergeBeyondRadius(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "a.py", 40, 40, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "a.py", 43, 43, pb.Severity_SEVERITY_HIGH, "bandit", "B608"), // 距离 3 > ±2
	}
	if gs := groupSegments(fs); len(gs) != 2 {
		t.Fatalf("want 2 groups, got %d", len(gs))
	}
}

// 区间端点扩展归并：[10,20] 与 22,22（22 ≤ 20+2）→ 同段
func TestGroupSegments_MergesViaEndLine(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "a.py", 10, 20, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "a.py", 22, 22, pb.Severity_SEVERITY_HIGH, "bandit", "B608"),
	}
	if gs := groupSegments(fs); len(gs) != 1 {
		t.Fatalf("want 1 group, got %d", len(gs))
	}
}

// 跨文件不归并
func TestGroupSegments_DifferentFilesSeparate(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "a.py", 42, 42, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "b.py", 42, 42, pb.Severity_SEVERITY_HIGH, "bandit", "B608"),
	}
	if gs := groupSegments(fs); len(gs) != 2 {
		t.Fatalf("want 2 groups, got %d", len(gs))
	}
}

// end_line 缺省（0/<start）按 start 计
func TestGroupSegments_EndLineDefault(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "a.py", 10, 0, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "a.py", 12, 0, pb.Severity_SEVERITY_HIGH, "bandit", "B608"), // 距离 2 → 归并
	}
	if gs := groupSegments(fs); len(gs) != 1 {
		t.Fatalf("want 1 group, got %d", len(gs))
	}
}

// 文件路径为空：各自独立成组（不参与归并）
func TestGroupSegments_EmptyFileStandalone(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("f1", "", 10, 10, pb.Severity_SEVERITY_HIGH, "semgrep", "r1"),
		mkFinding("f2", "", 10, 10, pb.Severity_SEVERITY_HIGH, "bandit", "B608"),
	}
	if gs := groupSegments(fs); len(gs) != 2 {
		t.Fatalf("want 2 groups, got %d", len(gs))
	}
}

// 代表选择：severity 最高者优先；同级取行号小者
func TestGroupSegments_RepresentativePick(t *testing.T) {
	fs := []*pb.UnifiedFinding{
		mkFinding("low", "a.py", 10, 10, pb.Severity_SEVERITY_LOW, "semgrep", "r1"),
		mkFinding("crit", "a.py", 11, 11, pb.Severity_SEVERITY_CRITICAL, "bandit", "B608"),
		mkFinding("crit2", "a.py", 12, 12, pb.Severity_SEVERITY_CRITICAL, "toolx", "rx"),
	}
	gs := groupSegments(fs)
	if len(gs) != 1 {
		t.Fatalf("want 1 group, got %d", len(gs))
	}
	if gs[0].Rep.GetFindingId() != "crit" {
		t.Fatalf("rep want crit (severity 最高、行号更小), got %s", gs[0].Rep.GetFindingId())
	}
}

// 判定广播：每成员一条，非代表带共用判定前缀，verdict/confidence 与基准一致
func TestBroadcastGroupVerdict(t *testing.T) {
	rep := mkFinding("rep", "a.py", 10, 10, pb.Severity_SEVERITY_HIGH, "semgrep", "r1")
	m2 := mkFinding("m2", "a.py", 11, 11, pb.Severity_SEVERITY_HIGH, "bandit", "B608")
	base := &pb.VerifiedFinding{
		OriginalFindingId: "rep",
		Verdict:           pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE,
		Confidence:        0.9,
		Reasoning:         "[DSH-sandbox] 数据流可达",
	}
	out := broadcastGroupVerdict(base, rep, []*pb.UnifiedFinding{rep, m2})
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[0] != base {
		t.Fatal("rep 成员应原样返回基准对象")
	}
	if out[1].GetOriginalFindingId() != "m2" {
		t.Fatalf("member id want m2, got %s", out[1].GetOriginalFindingId())
	}
	if out[1].GetVerdict() != base.GetVerdict() || out[1].GetConfidence() != base.GetConfidence() {
		t.Fatal("广播 verdict/confidence 应与基准一致")
	}
	if !strings.Contains(out[1].GetReasoning(), "同段判定（与 rep 共用一轮沙箱，ADR-186）") {
		t.Fatalf("广播 reasoning 缺共用前缀: %s", out[1].GetReasoning())
	}
}

// 附注生成：单条为空；多条含工具/规则清单
func TestSegmentPeersNote(t *testing.T) {
	single := segmentGroup{Rep: mkFinding("f1", "a.py", 1, 1, pb.Severity_SEVERITY_HIGH, "t", "r"),
		Members: []*pb.UnifiedFinding{mkFinding("f1", "a.py", 1, 1, pb.Severity_SEVERITY_HIGH, "t", "r")}}
	if segmentPeersNote(single) != "" {
		t.Fatal("单条发现附注应为空")
	}
	rep := mkFinding("rep", "a.py", 10, 10, pb.Severity_SEVERITY_HIGH, "semgrep", "r1")
	g := segmentGroup{Rep: rep, Members: []*pb.UnifiedFinding{rep,
		mkFinding("m2", "a.py", 11, 11, pb.Severity_SEVERITY_HIGH, "bandit", "B608")}}
	note := segmentPeersNote(g)
	if !strings.Contains(note, "bandit/B608") || !strings.Contains(note, "同文件同段") {
		t.Fatalf("附注应含工具/规则与同段说明: %s", note)
	}
}
