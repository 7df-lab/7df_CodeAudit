// CodeQL adapter parser (SARIF JSON output).
// 依据: 01 §5 CodeQL 口径 + codeaudit_common.proto L52-L94
package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type sarifResult struct {
	RuleID   string          `json:"ruleId"`
	Level    string          `json:"level"`
	Message  sarifMessage    `json:"message"`
	Location []sarifLocation `json:"locations"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	Physical sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	Artifact sarifArtifact `json:"artifactLocation"`
	Region   sarifRegion   `json:"region"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

type sarifOutput struct {
	Runs []sarifRun `json:"runs"`
}

type CodeQLAdapter struct{}

func init() { Register(CodeQLAdapter{}) }

func (CodeQLAdapter) ToolID() string { return "codeql" }
func (CodeQLAdapter) SupportedLanguages() []string {
	return []string{"python", "javascript", "typescript", "java", "go", "c", "cpp", "ruby", "csharp"}
}

func (CodeQLAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, fmt.Errorf("empty codeql output")
	}
	var out sarifOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid sarif json: %w", err)
	}
	start := time.Now()
	findings := make([]UnifiedFinding, 0)
	seq := 0
	for _, run := range out.Runs {
		for _, r := range run.Results {
			seq++
			filePath := ""
			startLine := 0
			endLine := 0
			startCol := 0
			endCol := 0
			if len(r.Location) > 0 {
				filePath = r.Location[0].Physical.Artifact.URI
				startLine = r.Location[0].Physical.Region.StartLine
				endLine = r.Location[0].Physical.Region.EndLine
				startCol = r.Location[0].Physical.Region.StartColumn
				endCol = r.Location[0].Physical.Region.EndColumn
			}
			findings = append(findings, UnifiedFinding{
				FindingID:    fmt.Sprintf("%s-codeql-%d", taskID, seq),
				TaskID:       taskID,
				ProjectID:    projectID,
				SourceTool:   "codeql",
				SourceRuleID: r.RuleID,
				SourceRaw:    raw,
				Location: ScanLocation{
					FilePath:    filePath,
					StartLine:   startLine,
					EndLine:     endLine,
					StartColumn: startCol,
					EndColumn:   endCol,
				},
				Title:       r.Message.Text,
				Description: r.Message.Text,
				Severity:    SeverityToProto(r.Level),
				RawSeverity: r.Level,
				Confidence:  0.8,
			})
		}
	}
	return BuildToolScanResult("codeql", findings, time.Since(start), 1), nil
}
