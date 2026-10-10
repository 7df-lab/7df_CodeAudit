package rulescan

import (
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// 依据: ADR-116 RuleScan 最小规则集
// 依据: 07 §10 降级策略矩阵 — LLM不可用→RuleScan产出结果

func TestEngineListRules(t *testing.T) {
	e := NewEngine()
	rules := e.ListRules()
	if len(rules) < 10 {
		t.Fatalf("expected at least 10 rules, got %d", len(rules))
	}
	// 验证关键规则存在
	expected := []string{"RULE-SQL-001", "RULE-CMD-001", "RULE-SECRET-001"}
	for _, want := range expected {
		found := false
		for _, r := range rules {
			if r == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected rule %s not found", want)
		}
	}
}

func TestEngineScanSQLInjection(t *testing.T) {
	e := NewEngine()
	// Go 代码中的 SQL 注入模式
	code := []byte(`query := fmt.Sprintf("SELECT * FROM users WHERE id=%s", req.Param)
rows, err := db.Query(query)`)
	findings := e.Scan("/project", "handler.go", code, nil)
	if len(findings) == 0 {
		t.Fatal("expected SQL injection finding")
	}
	found := false
	for _, f := range findings {
		if f.CweId == "CWE-89" {
			found = true
			if f.Severity != pb.Severity_SEVERITY_HIGH {
				t.Errorf("expected HIGH severity, got %v", f.Severity)
			}
			if f.SourceTool != "RuleScan" {
				t.Errorf("expected source_tool=RuleScan, got %s", f.SourceTool)
			}
		}
	}
	if !found {
		t.Fatal("expected CWE-89 finding")
	}
}

func TestEngineScanHardcodedSecret(t *testing.T) {
	e := NewEngine()
	code := []byte(`const apiKey = "sk-1234567890abcdef1234567890abcdef"`)
	findings := e.Scan("/project", "config.go", code, nil)
	found := false
	for _, f := range findings {
		if f.CweId == "CWE-798" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected hardcoded credential finding (CWE-798)")
	}
}

func TestEngineScanCleanCode(t *testing.T) {
	e := NewEngine()
	code := []byte(`func add(a, b int) int { return a + b }`)
	findings := e.Scan("/project", "math.go", code, nil)
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for clean code, got %d", len(findings))
	}
}

func TestEngineScanWithRuleFilter(t *testing.T) {
	e := NewEngine()
	code := []byte(`query := fmt.Sprintf("SELECT * FROM users WHERE id=%s", req.Param)
const password = "supersecretpassword1234567890"`)

	// 只请求 SQL 规则
	findings := e.Scan("/project", "handler.go", code, []string{"RULE-SQL-001"})
	for _, f := range findings {
		if f.SourceRuleId != "RULE-SQL-001" {
			t.Errorf("expected only RULE-SQL-001, got %s", f.SourceRuleId)
		}
	}
}

func TestEngineScanFileFilter(t *testing.T) {
	e := NewEngine()
	// Python 专用规则不应匹配 .go 文件
	code := []byte(`pickle.loads(data)`)
	findings := e.Scan("/project", "handler.go", code, nil)
	for _, f := range findings {
		if f.SourceRuleId == "RULE-DESER-001" {
			t.Error("pickle rule should not match .go files")
		}
	}

	// 应匹配 .py 文件
	findings = e.Scan("/project", "handler.py", code, nil)
	found := false
	for _, f := range findings {
		if f.SourceRuleId == "RULE-DESER-001" {
			found = true
		}
	}
	if !found {
		t.Error("pickle rule should match .py files")
	}
}

// 反向测试: 依据 test-gates.md §3 "LLM降级"行
// LLM 不可用时 RuleScan 仍产出结果
func TestRuleScanFallbackProducesResults(t *testing.T) {
	e := NewEngine()
	// 模拟 LLM 不可用场景：直接调用 RuleScan
	code := []byte(`query := fmt.Sprintf("SELECT * FROM users WHERE id=%s", userInput)
exec.Command("sh", "-c", userInput)
const api_key = "hardcoded-secret-key-1234567890"`)

	findings := e.Scan("/project", "vulnerable.go", code, nil)
	if len(findings) == 0 {
		t.Fatal("RuleScan fallback must produce results when LLM is unavailable (07 §10)")
	}

	// 验证至少覆盖多种 CWE
	cweSet := make(map[string]bool)
	for _, f := range findings {
		cweSet[f.CweId] = true
	}
	if len(cweSet) < 2 {
		t.Errorf("expected multiple CWE types, got %d: %v", len(cweSet), cweSet)
	}
}
