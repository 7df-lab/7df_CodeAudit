// Semgrep adapter parser (JSON output).
// 依据: 01 §5 Semgrep 口径 + codeaudit_common.proto L52-L94
package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type semgrepLoc struct {
	Line int `json:"line"`
	Col  int `json:"col"`
}

type semgrepMeta struct {
	CWE        interface{} `json:"cwe"`
	Confidence string      `json:"confidence"`
	Severity   string      `json:"severity"`
}

type semgrepExtra struct {
	Message  string      `json:"message"`
	Metadata semgrepMeta `json:"metadata"`
	Severity string      `json:"severity"`
	Lines    string      `json:"lines"`
	// ADR-158: OpenGrep 默认导出的变量级污点链路（taint_source/intermediate_vars/taint_sink）；
	// semgrep OSS 无此字段。原样透传进 per-finding source_raw，由前端渲染（proto FlowRole 语义）。
	DataflowTrace json.RawMessage `json:"dataflow_trace,omitempty"`
}

type semgrepResult struct {
	CheckID string       `json:"check_id"`
	Path    string       `json:"path"`
	Start   semgrepLoc   `json:"start"`
	End     semgrepLoc   `json:"end"`
	Extra   semgrepExtra `json:"extra"`
}

type semgrepOutput struct {
	Results []semgrepResult `json:"results"`
}

type SemgrepAdapter struct{}

func init() {
	Register(SemgrepAdapter{})
	Register(OpengrepAdapter{})
}

func (SemgrepAdapter) ToolID() string { return "semgrep" }
func (SemgrepAdapter) SupportedLanguages() []string {
	return []string{"python", "javascript", "typescript", "java", "go", "ruby"}
}

// OpengrepAdapter — ADR-158: semgrep 开源分叉 OpenGrep 的执行引擎适配器。
// 同 YAML 规则模式/同 JSON 输出 schema（Parse 复用 semgrep 家族解析），
// 差异能力=默认导出 dataflow_trace 变量级污点链路。
type OpengrepAdapter struct{}

func (OpengrepAdapter) ToolID() string { return "opengrep" }
func (OpengrepAdapter) SupportedLanguages() []string {
	return SemgrepAdapter{}.SupportedLanguages()
}

func (a SemgrepAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	return parseSemgrepFamily(a.ToolID(), taskID, projectID, raw)
}

func (a OpengrepAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	return parseSemgrepFamily(a.ToolID(), taskID, projectID, raw)
}

// parseSemgrepFamily — semgrep/OpenGrep 同 schema JSON 的共享解析（ADR-158）。
// toolID 决定 FindingID 前缀/SourceTool/统计口径；dataflow_trace 有则透传。
func parseSemgrepFamily(toolID, taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, fmt.Errorf("empty %s output", toolID)
	}
	var out semgrepOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid %s json: %w", toolID, err)
	}
	start := time.Now()
	findings := make([]UnifiedFinding, 0, len(out.Results))
	for i, r := range out.Results {
		cwe := normalizeCWESemgrep(r.Extra.Metadata.CWE)
		confidence := r.Extra.Metadata.Confidence
		if confidence == "" {
			confidence = r.Extra.Severity
		}
		severity := r.Extra.Severity
		if severity == "" {
			severity = r.Extra.Metadata.Severity
		}
		// ADR-141/142: per-finding 代码上下文（此前塞整份 run 输出）
		// ADR-143: 匹配点 ±10 行窗口
		payload := map[string]interface{}{
			"tool": toolID, "code": r.Extra.Lines, "line": r.Start.Line,
			"file": r.Path, "rule": r.CheckID,
		}
		if fc, ok := FileContextAt(r.Path, r.Start.Line, ContextRadius); ok {
			payload["context"] = json.RawMessage(ContextJSON(fc))
		}
		payload["taint"] = strings.Contains(r.CheckID, "taint") // ADR-144: 引擎确认 source→sink 可达
		// ADR-158: 变量级污点链路透传（OpenGrep 默认导出; proto EvidenceChain/FlowRole 的展示数据源）
		if len(r.Extra.DataflowTrace) > 0 {
			payload["dataflow_trace"] = json.RawMessage(r.Extra.DataflowTrace)
		}
		perFinding, _ := json.Marshal(payload)
		findings = append(findings, UnifiedFinding{
			FindingID:    fmt.Sprintf("%s-%s-%d", taskID, toolID, i+1),
			TaskID:       taskID,
			ProjectID:    projectID,
			SourceTool:   toolID,
			SourceRuleID: r.CheckID,
			SourceRaw:    perFinding,
			Location: ScanLocation{
				FilePath:    r.Path,
				StartLine:   r.Start.Line,
				EndLine:     r.End.Line,
				StartColumn: r.Start.Col,
				EndColumn:   r.End.Col,
			},
			CWEID:         cwe,
			Title:         r.Extra.Message,
			Description:   r.Extra.Message,
			Severity:      SeverityToProto(severity),
			RawSeverity:   severity,
			Confidence:    ConfidenceToProto(confidence),
			RawConfidence: confidence,
			RawLines:      r.Extra.Lines,
		})
	}
	return BuildToolScanResult(toolID, findings, time.Since(start), 1), nil
}

func normalizeCWESemgrep(v interface{}) string {
	switch vv := v.(type) {
	case []interface{}:
		for _, item := range vv {
			if s, ok := item.(string); ok {
				if id := extractCWEID(s); id != "" {
					return id
				}
			}
		}
	case string:
		if id := extractCWEID(vv); id != "" {
			return id
		}
	}
	return ""
}

func extractCWEID(s string) string {
	idx := -1
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "CWE-" || s[i:i+4] == "cwe-" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	end := idx + 4
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == idx+4 {
		return ""
	}
	return s[:end]
}
