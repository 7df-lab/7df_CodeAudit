package service

// ADR-225 增量聚焦提示词锁定测试：清单注入 / 零变更口径 / 超预算落文件+指路 /
// 任务卡含全量代码库指路（AI 跨文件追踪能力保留）。依据=伞仓验收文档 F8。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

func TestSandboxAssignmentIncremental_ListsAndGuides(t *testing.T) {
	a := sandboxAssignmentIncremental(
		[]string{"app.py", "pkg/new.py"}, []string{"old.py"}, "+++ app.py\n+xxx", "/tmp/proj")
	for _, want := range []string{
		"增量扫描重点",
		sandbox.ProjectSandboxPath, // 全量代码库指路：跨文件追踪能力保留
		"- app.py",
		"- pkg/new.py",
		"- old.py",
		"+++ app.py",
	} {
		if !strings.Contains(a, want) {
			t.Fatalf("任务卡缺 %q：\n%s", want, a)
		}
	}
	if !strings.Contains(a, sandboxAssignmentModeA()) {
		t.Fatalf("增量任务卡必须保留模式A 基底指令")
	}
}

func TestSandboxAssignmentIncremental_ZeroChange(t *testing.T) {
	a := sandboxAssignmentIncremental(nil, nil, "", "/tmp/proj")
	if !strings.Contains(a, "零代码变更") {
		t.Fatalf("零变更应如实声明：\n%s", a)
	}
}

func TestSandboxAssignmentIncremental_OversizeDiffToFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("+line\n", (incrementalPromptBudget/6)+100) // 必超 24KB 预算
	a := sandboxAssignmentIncremental([]string{"big.py"}, nil, big, dir)

	// 全文落项目树（随 tar 进沙箱，AI 可整文件读取）
	data, err := os.ReadFile(filepath.Join(dir, incrementalDiffFileName))
	if err != nil {
		t.Fatalf("超预算 diff 未落文件: %v", err)
	}
	if len(data) != len(big) {
		t.Fatalf("落盘 diff 被截断（%d != %d）", len(data), len(big))
	}
	// 任务卡：截断内联 + 沙箱内路径指路
	if !strings.Contains(a, "已截断") {
		t.Fatalf("任务卡缺截断声明")
	}
	if !strings.Contains(a, filepath.Join(sandbox.ProjectSandboxPath, incrementalDiffFileName)) {
		t.Fatalf("任务卡缺沙箱内 diff 文件指路：\n%s", a)
	}
}

func TestTruncateAtLine_LineBoundary(t *testing.T) {
	in := "a\nbb\nccc\ndddd\n"
	got := truncateAtLine(in, 5)
	if strings.HasSuffix(got, "\n") || len(got) > 5 {
		t.Fatalf("截断应在行边界且不含尾部换行: %q", got)
	}
	if got != "a\nbb" {
		t.Fatalf("truncateAtLine=%q", got)
	}
}

// A8.3（验收 F8 补齐，2026-09-11）: 非增量请求的 Assignment 必须与现状逐字节一致——
// 分支判据（任一增量字段非空才走增量构造）的回归快照锁：空增量字段时构造器输出
// 与既有 ModeA 模板逐字相同（含增量字段时的行为差异由上方四例锁定）。
func TestSandboxAssignment_NonIncrementalUnchanged(t *testing.T) {
	// 生产判据锁定（ai_engine 消费 isIncrementalAssignment）：全空字段 → 走 ModeA 现状
	if isIncrementalAssignment(nil, nil, "") {
		t.Fatal("empty incremental fields must keep verbatim ModeA assignment")
	}
	for _, c := range []struct {
		name              string
		changed, deleted  []string
		diff              string
	}{
		{"changed only", []string{"a.py"}, nil, ""},
		{"deleted only", nil, []string{"b.py"}, ""},
		{"diff only", nil, nil, "diff-text"},
	} {
		if !isIncrementalAssignment(c.changed, c.deleted, c.diff) {
			t.Fatalf("%s: must switch to incremental assignment", c.name)
		}
	}
	// 基底字面锚：增量任务卡以 ModeA 全文为前缀（增量段是追加，不替换既有任务卡）
	modeA := sandboxAssignmentModeA()
	inc := sandboxAssignmentIncremental([]string{"a.py"}, nil, "", "")
	if !strings.HasPrefix(inc, modeA) {
		t.Fatal("incremental assignment must contain the full ModeA base")
	}
}
