// Package rulescan implements the RuleScan fallback engine.
// 依据: 07 §10 降级策略矩阵 — "LLM 不可用 → 降级规则引擎（RuleScan）"
// 依据: ADR-116 RuleScan 最小规则集（CWE Top25 基础模式）
//
// ADR-175（2026-09-01 人类裁决）：ai-inference 服务整体删除（九服务→八服务，
// Chat 直连门面自 ADR-140 起即死代码），本引擎自 ai-inference 内嵌至 dsh-runtime——
// 沙箱 DSH 不可用时的降级终点本地化（省一次 gRPC 拨号）。
// 降级诚实标注（人类硬性要求）：经本引擎产出的发现必须 ai_verdict=NEEDS_MANUAL
// 且 source_rule_id 带 rulescan-fallback 前缀，不得冒充 AI 语义分析结果。
package rulescan

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"encoding/json"
	"strings"
	"sync"

	pb "github.com/codeaudit/proto-gen"
)

// Rule represents a static analysis rule.
// 依据: ADR-116 — 内置 CWE Top 25 高频模式
type Rule struct {
	ID          string
	CWEID       string
	Title       string
	Description string
	Severity    pb.Severity
	Patterns    []*regexp.Regexp // 正则模式匹配
	FileFilter  string           // 文件扩展名过滤（空=全部）
}

// Engine is the RuleScan static analysis engine.
// 依据: 07 §10 — LLM不可用时的结果兜底机制
type Engine struct {
	mu    sync.RWMutex
	rules []*Rule
}

// NewEngine creates a new RuleScan engine with built-in rules.
// 依据: ADR-116 — CWE Top 25 基础规则集
func NewEngine() *Engine {
	e := &Engine{}
	e.loadBuiltinRules()
	return e
}

// loadBuiltinRules loads the minimal CWE Top 25 rule set.
// 依据: ADR-116 — 10条基础规则
func (e *Engine) loadBuiltinRules() {
	e.rules = []*Rule{
		// CWE-89: SQL Injection
		{
			ID: "RULE-SQL-001", CWEID: "CWE-89",
			Title: "Potential SQL Injection", Severity: pb.Severity_SEVERITY_HIGH,
			Patterns: compilePatterns([]string{
				`(?i)(SELECT|INSERT|UPDATE|DELETE)\s+.*\+.*(?:req|r)\.`,
				`(?i)fmt\.Sprintf\(.*(?:SELECT|INSERT|UPDATE|DELETE)`,
				`(?i)db\.(?:Query|Exec)\(.*\+`,
			}),
			FileFilter: ".go,.py,.java,.js,.ts",
		},
		// CWE-78: Command Injection
		{
			ID: "RULE-CMD-001", CWEID: "CWE-78",
			Title: "Potential Command Injection", Severity: pb.Severity_SEVERITY_CRITICAL,
			Patterns: compilePatterns([]string{
				`os\.exec\.Command\(.*\+`,
				`subprocess\.(?:call|run|Popen)\(.*\+`,
				`Runtime\.getRuntime\(\)\.exec\(.*\+`,
			}),
			FileFilter: ".go,.py,.java",
		},
		// CWE-79: Cross-site Scripting (XSS)
		{
			ID: "RULE-XSS-001", CWEID: "CWE-79",
			Title: "Potential XSS via Unescaped Output", Severity: pb.Severity_SEVERITY_HIGH,
			Patterns: compilePatterns([]string{
				`innerHTML\s*=\s*[^"']`,
				`document\.write\(.*\+`,
				`\|\s*safe\b`,
			}),
			FileFilter: ".js,.jsx,.ts,.tsx,.html,.py",
		},
		// CWE-22: Path Traversal
		{
			ID: "RULE-PATH-001", CWEID: "CWE-22",
			Title: "Potential Path Traversal", Severity: pb.Severity_SEVERITY_HIGH,
			Patterns: compilePatterns([]string{
				`os\.Open\(.*\+`,
				`filepath\.Join\(.*req\.`,
				`open\(.*\+.*request`,
			}),
			FileFilter: ".go,.py,.java",
		},
		// CWE-798: Hard-coded Credentials
		{
			ID: "RULE-SECRET-001", CWEID: "CWE-798",
			Title: "Hard-coded Credential", Severity: pb.Severity_SEVERITY_CRITICAL,
			Patterns: compilePatterns([]string{
				`(?i)(?:password|secret|api_?[Kk]ey|token)\s*[:=]\s*["'][^"']{8,}["']`,
				`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`,
			}),
			FileFilter: "",
		},
		// CWE-287: Improper Authentication
		{
			ID: "RULE-AUTH-001", CWEID: "CWE-287",
			Title: "Missing Authentication Check", Severity: pb.Severity_SEVERITY_HIGH,
			Patterns: compilePatterns([]string{
				`(?i)// ?TODO.*auth`,
				`(?i)// ?FIXME.*auth`,
				`auth\s*[:=]\s*(?:nil|None|null|false)`,
			}),
			FileFilter: "",
		},
		// CWE-502: Deserialization of Untrusted Data
		{
			ID: "RULE-DESER-001", CWEID: "CWE-502",
			Title: "Unsafe Deserialization", Severity: pb.Severity_SEVERITY_CRITICAL,
			Patterns: compilePatterns([]string{
				`pickle\.loads?\(`,
				`yaml\.load\([^)]*(?!Loader)`,
				`ObjectInputStream\(`,
			}),
			FileFilter: ".py,.java",
		},
		// CWE-918: Server-Side Request Forgery (SSRF)
		{
			ID: "RULE-SSRF-001", CWEID: "CWE-918",
			Title: "Potential SSRF", Severity: pb.Severity_SEVERITY_HIGH,
			Patterns: compilePatterns([]string{
				`http\.(?:Get|Post)\(.*req\.`,
				`requests\.(?:get|post)\(.*request\.`,
				`URL\(.*req\.`,
			}),
			FileFilter: ".go,.py,.java,.js",
		},
		// CWE-476: NULL Pointer Dereference
		{
			ID: "RULE-NULL-001", CWEID: "CWE-476",
			Title: "Potential Nil Pointer Dereference", Severity: pb.Severity_SEVERITY_MEDIUM,
			Patterns: compilePatterns([]string{
				`\.\w+\.\w+\(\)(?!\s*(?:!=|==)\s*nil)`,
			}),
			FileFilter: ".go",
		},
		// CWE-125: Out-of-bounds Read
		{
			ID: "RULE-OOB-001", CWEID: "CWE-125",
			Title: "Potential Out-of-bounds Access", Severity: pb.Severity_SEVERITY_MEDIUM,
			Patterns: compilePatterns([]string{
				`\[(?:i|j|k|index|offset)\](?!\s*(?:<|>|<=|>=|==))`,
			}),
			FileFilter: ".go,.c,.cpp",
		},
	}
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	var compiled []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err == nil {
			compiled = append(compiled, re)
		}
	}
	return compiled
}

// Scan performs a rule-based scan on source code content.
// 依据: codeaudit_common.proto L1361 RuleScanRequest — project_path + rule_ids
// 依据: 07 §10 — LLM降级时仍产出结果
func (e *Engine) Scan(projectPath string, sourceFile string, content []byte, ruleIDs []string) []*pb.UnifiedFinding {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var findings []*pb.UnifiedFinding
	contentStr := string(content)
	lines := strings.Split(contentStr, "\n")
	ext := filepath.Ext(sourceFile)

	for _, rule := range e.rules {
		// Filter by requested rule_ids
		if len(ruleIDs) > 0 {
			found := false
			for _, rid := range ruleIDs {
				if rid == rule.ID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Filter by file extension
		if rule.FileFilter != "" {
			allowed := false
			for _, e := range strings.Split(rule.FileFilter, ",") {
				if ext == e {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}

		// Pattern matching
		for lineNum, line := range lines {
			for _, pattern := range rule.Patterns {
				if pattern.MatchString(line) {
					// ADR-141: 匹配行文本随发现走（source_raw 既有字段语义）——复核页面有代码可看
					// ADR-143: ±10 行上下文窗口（引擎已持有文件行数组，直接切片）
					ctxObj := map[string]interface{}{}
					s := lineNum - 10
					if s < 0 {
						s = 0
					}
					e := lineNum + 10
					if e > len(lines) {
						e = len(lines)
					}
					if s < e {
						ctxObj = map[string]interface{}{
							"start_line": s + 1, "end_line": e,
							"lines": lines[s:e],
						}
					}
					rawJSON, _ := json.Marshal(map[string]interface{}{
						"tool":    "rulescan",
						"code":    strings.TrimRight(line, "\r\n"),
						"line":    lineNum + 1,
						"file":    sourceFile,
						"context": ctxObj,
					})
					finding := &pb.UnifiedFinding{
						FindingId:    rule.ID + "-" + sourceFile + "-" + strconv.Itoa(lineNum+1),
						SourceTool:   "RuleScan",
						SourceRuleId: rule.ID,
						CweId:        rule.CWEID,
						Title:        rule.Title,
						Description:  rule.Description,
						Severity:     rule.Severity,
						Confidence:   0.6, // 规则引擎置信度低于LLM
						SourceRaw:    rawJSON,
						Location: &pb.LocationInfo{
							FilePath:  sourceFile,
							StartLine: int32(lineNum + 1),
							EndLine:   int32(lineNum + 1),
						},
					}
					findings = append(findings, finding)
				}
			}
		}
	}
	return findings
}

// ListRules returns all available rule IDs.
func (e *Engine) ListRules() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ids := make([]string, len(e.rules))
	for i, r := range e.rules {
		ids[i] = r.ID
	}
	return ids
}

// ScanDirectory scans all files in a directory.
func (e *Engine) ScanDirectory(projectPath string, ruleIDs []string) ([]*pb.UnifiedFinding, error) {
	var allFindings []*pb.UnifiedFinding

	err := filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无法访问的文件
		}
		if info.IsDir() {
			return nil
		}
		// 跳过隐藏目录和 vendor
		rel, _ := filepath.Rel(projectPath, path)
		if strings.HasPrefix(rel, ".") || strings.Contains(rel, "vendor/") || strings.Contains(rel, "node_modules/") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// 限制文件大小（10MB）
		if len(content) > 10*1024*1024 {
			return nil
		}

		sourceFile, _ := filepath.Rel(projectPath, path)
		findings := e.Scan(projectPath, sourceFile, content, ruleIDs)
		allFindings = append(allFindings, findings...)
		return nil
	})

	return allFindings, err
}
