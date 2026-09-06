// Example JSON adapter for generic rule-style SAST output.
// 依据: codeaudit_common.proto L52-L94 (UnifiedFinding mapping)
// 依据: 01 §5 十适配器口径（自定义适配器示例）
package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type jsonRuleLocation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	StartCol  int    `json:"start_col"`
	EndCol    int    `json:"end_col"`
	Function  string `json:"function"`
	Class     string `json:"class"`
}

type jsonRuleFinding struct {
	RuleID      string           `json:"rule_id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Severity    string           `json:"severity"`
	Confidence  string           `json:"confidence"`
	CWE         string           `json:"cwe"`
	Location    jsonRuleLocation `json:"location"`
	RawPayload  json.RawMessage  `json:"raw_payload"`
}

type jsonRuleOutput struct {
	Tool     string            `json:"tool"`
	Findings []jsonRuleFinding `json:"findings"`
}

// JSONAdapter parses a normalized JSON output format.
// This adapter exists to prove the framework end-to-end for TP06-T1.
// 依据: roadmap TP06-T1 完成标准（框架单测+1示例适配器）
type JSONAdapter struct{}

func init() {
	Register(JSONAdapter{})
}

func (JSONAdapter) ToolID() string {
	return "json"
}

func (JSONAdapter) SupportedLanguages() []string {
	return []string{"any"}
}

func (JSONAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, errors.New("empty raw output")
	}
	var out jsonRuleOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid json output: %w", err)
	}
	if out.Tool == "" {
		return ToolScanResult{}, errors.New("tool is required")
	}

	start := time.Now()
	findings := make([]UnifiedFinding, 0, len(out.Findings))
	for i, f := range out.Findings {
		uf := UnifiedFinding{
			FindingID:    fmt.Sprintf("%s-json-%d", taskID, i+1),
			TaskID:       taskID,
			ProjectID:    projectID,
			SourceTool:   out.Tool,
			SourceRuleID: f.RuleID,
			SourceRaw:    mustMarshal(f.RawPayload),
			Location: ScanLocation{
				FilePath:     f.Location.Path,
				StartLine:    f.Location.StartLine,
				EndLine:      f.Location.EndLine,
				StartColumn:  f.Location.StartCol,
				EndColumn:    f.Location.EndCol,
				FunctionName: f.Location.Function,
				ClassName:    f.Location.Class,
			},
			CWEID:         f.CWE,
			Title:         f.Title,
			Description:   f.Description,
			Severity:      SeverityToProto(f.Severity),
			RawSeverity:   f.Severity,
			Confidence:    ConfidenceToProto(f.Confidence),
			RawConfidence: f.Confidence,
		}
		findings = append(findings, uf)
	}

	return BuildToolScanResult(out.Tool, findings, time.Since(start), 1), nil
}

func mustMarshal(v json.RawMessage) []byte {
	if len(v) == 0 {
		return []byte("{}")
	}
	return v
}
