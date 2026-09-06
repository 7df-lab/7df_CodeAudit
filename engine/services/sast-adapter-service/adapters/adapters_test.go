package adapters

import (
	"encoding/json"
	"os"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../tests/fixtures/" + name)
	if err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	return raw
}

func TestGetParserUnknownTool(t *testing.T) {
	_, err := GetParser("not-exist")
	if err == nil {
		t.Fatalf("expected error for unknown tool")
	}
}

func TestValidateRequiredRejectsEmptySource(t *testing.T) {
	err := ValidateRequired(UnifiedFinding{})
	if err == nil {
		t.Fatalf("expected validation error for empty source_tool")
	}
}

func TestValidateRequiredRejectsBadLocation(t *testing.T) {
	err := ValidateRequired(UnifiedFinding{SourceTool: "x", Location: ScanLocation{FilePath: "a.go"}})
	if err == nil {
		t.Fatalf("expected validation error for start_line<=0")
	}
}

func TestJSONAdapterParseMinimal(t *testing.T) {
	raw := loadFixture(t, "adapter_json_sample.json")
	parser, err := GetParser("json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].SourceTool != "json" {
		t.Fatalf("unexpected source_tool: %s", res.Findings[0].SourceTool)
	}
	if res.Findings[0].Location.StartLine != 7 {
		t.Fatalf("unexpected start_line: %d", res.Findings[0].Location.StartLine)
	}
	if res.Findings[0].Severity != "SEVERITY_HIGH" {
		t.Fatalf("unexpected severity: %s", res.Findings[0].Severity)
	}
	if res.Findings[0].CWEID != "CWE-89" {
		t.Fatalf("unexpected cwe: %s", res.Findings[0].CWEID)
	}
}

func TestJSONAdapterGoldenComparison(t *testing.T) {
	raw := loadFixture(t, "adapter_json_sample.json")
	goldenRaw, err := os.ReadFile("../../../tests/golden/adapter_json_example.json")
	if err != nil {
		t.Skipf("golden not present: %v", err)
	}
	parser, _ := GetParser("json")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}

	var golden struct {
		ToolName string `json:"tool_name"`
		Findings []struct {
			SourceTool   string  `json:"source_tool"`
			SourceRuleID string  `json:"source_rule_id"`
			LocationFile string  `json:"location_file_path"`
			StartLine    int     `json:"location_start_line"`
			EndLine      int     `json:"location_end_line"`
			StartColumn  int     `json:"location_start_column"`
			EndColumn    int     `json:"location_end_column"`
			CWEID        string  `json:"cwe_id"`
			Title        string  `json:"title"`
			Severity     string  `json:"severity"`
			Confidence   float32 `json:"confidence"`
		} `json:"findings"`
		Metrics struct {
			FilesScanned  int32            `json:"files_scanned"`
			FindingsCount int32            `json:"findings_count"`
			BySeverity    map[string]int32 `json:"by_severity"`
		} `json:"metrics"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		t.Fatal(err)
	}

	if res.ToolName != golden.ToolName {
		t.Fatalf("tool_name mismatch: %s vs %s", res.ToolName, golden.ToolName)
	}
	if len(res.Findings) != len(golden.Findings) {
		t.Fatalf("findings len mismatch: %d vs %d", len(res.Findings), len(golden.Findings))
	}

	f := res.Findings[0]
	g := golden.Findings[0]
	assertEq := func(field string, a, b any) {
		t.Helper()
		if a != b {
			t.Fatalf("%s mismatch: %v vs %v", field, a, b)
		}
	}
	assertEq("source_tool", f.SourceTool, g.SourceTool)
	assertEq("source_rule_id", f.SourceRuleID, g.SourceRuleID)
	assertEq("location_file_path", f.Location.FilePath, g.LocationFile)
	assertEq("location_start_line", f.Location.StartLine, g.StartLine)
	assertEq("location_end_line", f.Location.EndLine, g.EndLine)
	assertEq("location_start_column", f.Location.StartColumn, g.StartColumn)
	assertEq("location_end_column", f.Location.EndColumn, g.EndColumn)
	assertEq("cwe_id", f.CWEID, g.CWEID)
	assertEq("title", f.Title, g.Title)
	assertEq("severity", f.Severity, g.Severity)
	assertEq("confidence", f.Confidence, g.Confidence)
	assertEq("metrics.files_scanned", res.Metrics.FilesScanned, golden.Metrics.FilesScanned)
	assertEq("metrics.findings_count", res.Metrics.FindingsCount, golden.Metrics.FindingsCount)
	assertEq("metrics.by_severity.SEVERITY_HIGH", res.Metrics.BySeverity["SEVERITY_HIGH"], golden.Metrics.BySeverity["SEVERITY_HIGH"])
	assertEq("status", res.Status, golden.Status)
}

func TestESLintAdapterParsesSample(t *testing.T) {
	raw := loadFixture(t, "eslint_sample.json")
	parser, _ := GetParser("eslint")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 eslint finding, got %d", len(res.Findings))
	}
	if res.Findings[0].SourceTool != "eslint" {
		t.Fatalf("unexpected source_tool: %s", res.Findings[0].SourceTool)
	}
	// fixture eslint_sample.json 的 message.line=6
	if res.Findings[0].Location.StartLine != 6 {
		t.Fatalf("unexpected line: %d", res.Findings[0].Location.StartLine)
	}
	if res.Findings[0].Severity != "SEVERITY_HIGH" {
		t.Fatalf("unexpected severity: %s", res.Findings[0].Severity)
	}
}

func TestSemgrepAdapterParsesSample(t *testing.T) {
	raw := loadFixture(t, "semgrep_sample.json")
	parser, _ := GetParser("semgrep")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 semgrep finding, got %d", len(res.Findings))
	}
	if res.Findings[0].SourceRuleID == "" {
		t.Fatalf("expected non-empty rule id")
	}
	if res.Findings[0].CWEID != "CWE-89" {
		t.Fatalf("expected CWE-89, got %s", res.Findings[0].CWEID)
	}
}

func TestBanditAdapterParsesSample(t *testing.T) {
	raw := loadFixture(t, "bandit_sample.json")
	parser, _ := GetParser("bandit")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 bandit finding, got %d", len(res.Findings))
	}
	// fixture bandit_sample.json 的 issue_severity=MEDIUM, 经 SeverityToProto 映射为 SEVERITY_MEDIUM
	if res.Findings[0].Severity != "SEVERITY_MEDIUM" {
		t.Fatalf("unexpected severity: %s", res.Findings[0].Severity)
	}
}

func TestCodeQLAdapterParsesSample(t *testing.T) {
	raw := loadFixture(t, "codeql_sample.sarif")
	parser, _ := GetParser("codeql")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 codeql finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Location.StartLine != 7 {
		t.Fatalf("unexpected line: %d", res.Findings[0].Location.StartLine)
	}
}

func TestSpotBugsAdapterParsesSample(t *testing.T) {
	raw := loadFixture(t, "spotbugs_sample.xml")
	parser, _ := GetParser("spotbugs")
	res, err := parser.Parse("task-1", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 spotbugs findings, got %d", len(res.Findings))
	}
	if res.Findings[0].Severity != "SEVERITY_CRITICAL" {
		t.Fatalf("unexpected severity: %s", res.Findings[0].Severity)
	}
}

// TestOpengrepAdapterDataflowTrace — ADR-158：opengrep 引擎的变量级污点链路
// （extra.dataflow_trace: taint_source/intermediate_vars/taint_sink）必须透传进
// per-finding source_raw，供前端按 proto FlowRole 语义渲染（SOURCE/PROPAGATION/SINK）。
func TestOpengrepAdapterDataflowTrace(t *testing.T) {
	raw := []byte(`{"results":[{"check_id":"codeaudit-sql-taint-user-param","path":"app.py",
		"start":{"line":6,"col":5},"end":{"line":6,"col":26},
		"extra":{"message":"污点传播","severity":"ERROR","lines":"cursor.execute(query)",
		"dataflow_trace":{"taint_source":["CliLoc",[{"path":"app.py","start":{"line":4,"col":5},"end":{"line":4,"col":56}},"query = \"SELECT * FROM users WHERE id = \" + user_id"]],
		"intermediate_vars":[{"location":{"path":"app.py","start":{"line":4,"col":5},"end":{"line":4,"col":10}},"content":"query"}],
		"taint_sink":["CliLoc",[{"path":"app.py","start":{"line":6,"col":5},"end":{"line":6,"col":26}},"cursor.execute(query)"]]}}}]}`)
	parser, err := GetParser("opengrep")
	if err != nil {
		t.Fatalf("opengrep parser not registered: %v", err)
	}
	res, err := parser.Parse("task-og", "proj-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	f := res.Findings[0]
	if f.SourceTool != "opengrep" || f.FindingID != "task-og-opengrep-1" {
		t.Fatalf("unexpected identity: tool=%s id=%s", f.SourceTool, f.FindingID)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(f.SourceRaw, &payload); err != nil {
		t.Fatal(err)
	}
	trace, ok := payload["dataflow_trace"].(map[string]interface{})
	if !ok {
		t.Fatalf("dataflow_trace not passed through in source_raw: %s", f.SourceRaw)
	}
	if _, ok := trace["intermediate_vars"]; !ok {
		t.Fatalf("intermediate_vars missing from trace: %s", f.SourceRaw)
	}
	if taint, _ := payload["taint"].(bool); !taint {
		t.Fatalf("taint flag lost")
	}
}
