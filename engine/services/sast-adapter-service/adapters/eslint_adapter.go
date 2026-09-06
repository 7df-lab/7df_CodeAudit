// ESLint adapter parser (JSON output).
// 依据: 01 §5 ESLint 口径 + codeaudit_common.proto L52-L94
// 依据: tests/fixtures/eslint_sample.json
package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

type eslintMessage struct {
	RuleID   string `json:"ruleId"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	EndLine  int    `json:"endLine"`
	EndCol   int    `json:"endColumn"`
}

type eslintFile struct {
	FilePath string          `json:"filePath"`
	Messages []eslintMessage `json:"messages"`
}

type ESLintAdapter struct{}

func init() { Register(ESLintAdapter{}) }

func (ESLintAdapter) ToolID() string               { return "eslint" }
func (ESLintAdapter) SupportedLanguages() []string { return []string{"javascript", "typescript"} }

func (ESLintAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, fmt.Errorf("empty eslint output")
	}
	var files []eslintFile
	if err := json.Unmarshal(raw, &files); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid eslint json: %w", err)
	}
	start := time.Now()
	findings := make([]UnifiedFinding, 0)
	seq := 0
	for _, f := range files {
		for _, m := range f.Messages {
			seq++
			severity := "SEVERITY_MEDIUM"
			if m.Severity == 2 {
				severity = "SEVERITY_HIGH"
			}
			findings = append(findings, UnifiedFinding{
				FindingID:    fmt.Sprintf("%s-eslint-%d", taskID, seq),
				TaskID:       taskID,
				ProjectID:    projectID,
				SourceTool:   "eslint",
				SourceRuleID: m.RuleID,
				SourceRaw:    raw,
				Location: ScanLocation{
					FilePath:    f.FilePath,
					StartLine:   m.Line,
					EndLine:     m.EndLine,
					StartColumn: m.Column,
					EndColumn:   m.EndCol,
				},
				Title:       m.Message,
				Description: m.Message,
				Severity:    severity,
				Confidence:  0.7,
			})
		}
	}
	return BuildToolScanResult("eslint", findings, time.Since(start), int32(len(files))), nil
}
