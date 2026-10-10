package service

// 沙箱发现映射回归（人类需求"AI 结论进当前结论列 + 代码上下文"）：
// AiVerdict/AiConfidence 由映射写入、reasoning 带 [DSH-sandbox] 前缀、
// source_raw 为 ADR-143 schema 的 base64（±10 行真实文件内容）、越界/穿越如实留空。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

func TestMapSandboxFindings_VerdictAxesAndReasoning(t *testing.T) {
	got := mapSandboxFindings("t-1", "", []sandbox.Finding{{
		Title: "SQL 注入", Severity: "SEVERITY_CRITICAL", CweID: "CWE-89",
		FilePath: "app.py", StartLine: 16, Confidence: 0.98,
		Reasoning: "直接拼接 user_id 构造 SQL",
	}})
	if len(got) != 1 {
		t.Fatalf("mapped=%d", len(got))
	}
	f := got[0]
	if f.GetVerdictDirection() != pb.VerdictDirection_VERDICT_DIRECTION_TRUE || f.GetVerdictTier() != pb.VerdictTier_VERDICT_TIER_LIKELY {
		t.Fatalf("verdict axes must be (TRUE, LIKELY), got (%v, %v)", f.GetVerdictDirection(), f.GetVerdictTier())
	}
	if f.GetAiConfidence() != 0.98 {
		t.Fatalf("ai_confidence must carry model confidence, got %v", f.GetAiConfidence())
	}
	if !strings.HasPrefix(f.GetAiReasoning(), "[DSH-sandbox] ") {
		t.Fatalf("reasoning must carry AI source prefix: %q", f.GetAiReasoning())
	}
	if f.GetLocation().GetFilePath() != "app.py" || f.GetLocation().GetStartLine() != 16 {
		t.Fatalf("location: %+v", f.GetLocation())
	}
}

func TestCaptureCodeContext_WindowAndSchema(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		b.WriteString("line" + itoa(i) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	dec := captureCodeContext(dir, "app.py", 16)
	if dec == nil {
		t.Fatal("context must be captured")
	}
	var obj struct {
		Code    string `json:"code"`
		Line    int    `json:"line"`
		Context struct {
			StartLine int      `json:"start_line"`
			EndLine   int      `json:"end_line"`
			Lines     []string `json:"lines"`
		} `json:"context"`
	}
	if err := json.Unmarshal(dec, &obj); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if obj.Code != "line16" || obj.Line != 16 {
		t.Fatalf("match line: %+v", obj)
	}
	if obj.Context.StartLine != 6 || obj.Context.EndLine != 26 || len(obj.Context.Lines) != 21 {
		t.Fatalf("±10 window: %+v", obj.Context)
	}
	if obj.Context.Lines[10] != "line16" {
		t.Fatalf("window must contain match at offset 10: %q", obj.Context.Lines[10])
	}
}

func TestCaptureCodeContext_HonestEmpty(t *testing.T) {
	dir := t.TempDir()
	if raw := captureCodeContext(dir, "missing.py", 3); raw != nil {
		t.Fatalf("missing file must leave source_raw empty, got %q", raw)
	}
	if raw := captureCodeContext(dir, "app.py", 0); raw != nil {
		t.Fatal("invalid line must leave source_raw empty")
	}
	// 路径穿越清洗：../ 逃逸不得读到项目外文件
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw := captureCodeContext(dir, "../../"+filepath.Base(outside)+"/secret.txt", 1); raw != nil {
		t.Fatalf("path traversal must not leak file content, got %q", raw)
	}
}

// itoa — 测试内整数转字符串（与 ADR-167 前例一致，避免仅为测试引别名）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// R135（缺损批 A6 露牙）：内容稳定 ID——同发现跨纪元重报必须同 ID（原序号每纪元
// 从 1 重编=epoch-2 主键碰撞全败，result 日志 22×duplicate key 实证）；异发现必异 ID。
func TestMapSandboxFindings_StableContentIDs(t *testing.T) {
	fs := []sandbox.Finding{
		{Title: "SQL 注入", Description: "d1", Severity: "SEVERITY_HIGH", FilePath: "app.py", StartLine: 5, CweID: "CWE-89"},
		{Title: "SSRF", Description: "d2", Severity: "SEVERITY_HIGH", FilePath: "app.py", StartLine: 9, CweID: "CWE-918"},
	}
	a := mapSandboxFindings("t-stable", "", fs)
	b := mapSandboxFindings("t-stable", "", fs) // epoch-2 重报（同内容）
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("len a=%d b=%d", len(a), len(b))
	}
	for i := range a {
		if a[i].GetFindingId() == "" {
			t.Fatal("ID 未回填")
		}
		if a[i].GetFindingId() != b[i].GetFindingId() {
			t.Fatalf("同内容跨纪元 ID 漂移（主键碰撞复发）: %s vs %s", a[i].GetFindingId(), b[i].GetFindingId())
		}
		if !strings.HasPrefix(a[i].GetFindingId(), "t-stable-sbx-") || len(a[i].GetFindingId()) != len("t-stable-sbx-")+12 {
			t.Fatalf("ID 形态失范（应为 taskID-sbx-<12hex>）: %s", a[i].GetFindingId())
		}
	}
	if a[0].GetFindingId() == a[1].GetFindingId() {
		t.Fatal("异发现同 ID（内容摘要区分度失效）")
	}
	// F5：DedupGroup=无行号摘要——同文件同 CWE 行号漂移归组（融合视图分组锚），
	// 异 CWE 异组；未定位回落无行号键。
	shifted := mapSandboxFindings("t-stable", "", []sandbox.Finding{
		{Title: "SQL 注入", Description: "d1", Severity: "SEVERITY_HIGH", FilePath: "app.py", StartLine: 7, CweID: "CWE-89"},
	})
	if shifted[0].GetDedupGroup() == "" || shifted[0].GetDedupGroup() != a[0].GetDedupGroup() {
		t.Fatalf("同 file:cwe 行号漂移应同组: shifted=%q a0=%q", shifted[0].GetDedupGroup(), a[0].GetDedupGroup())
	}
	if a[0].GetDedupGroup() == a[1].GetDedupGroup() {
		t.Fatal("异 CWE 应异组")
	}
}
