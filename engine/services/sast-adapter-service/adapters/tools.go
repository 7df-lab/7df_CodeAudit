// Package adapters implements the CodeAudit SAST adapter framework.
// 依据: codeaudit_common.proto L52-L94 (UnifiedFinding)
// 依据: codeaudit_common.proto L347-L361 (ToolScanResult/ScanMetrics)
// 依据: 01 §5 传统SAST 十适配器口径（此文件提供框架+注册表）
package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ScanLocation mirrors UnifiedFinding.location for adapter parsing.
// 依据: codeaudit_common.proto L38-L47
type ScanLocation struct {
	FilePath     string
	StartLine    int
	EndLine      int
	StartColumn  int
	EndColumn    int
	FunctionName string
	ClassName    string
	ModuleName   string
}

// UnifiedFinding is the adapter-layer model for SAST parsed output.
// 依据: codeaudit_common.proto L52-L94
type UnifiedFinding struct {
	FindingID     string
	TaskID        string
	ProjectID     string
	SourceTool    string
	SourceRuleID  string
	SourceRaw     []byte
	Location      ScanLocation
	CWEID         string
	Title         string
	Description   string
	Severity      string
	Confidence    float32
	RawSeverity   string
	RawConfidence string
	RawLines      string
	RawMoreInfo   string
}

// ScanMetrics is the minimal adapter scan metrics.
// 依据: codeaudit_common.proto L355-L361
type ScanMetrics struct {
	DurationMs    int64
	FilesScanned  int32
	LinesScanned  int32
	FindingsCount int32
	BySeverity    map[string]int32
}

// ToolScanResult is parsed result for one tool.
// 依据: codeaudit_common.proto L347-L353
type ToolScanResult struct {
	ToolName     string
	Findings     []UnifiedFinding
	Metrics      ScanMetrics
	Status       string
	ErrorMessage string
}

// Parser is the adapter interface for parsing raw tool output.
// 依据: 01 §5 10适配器口径 + 04 §3.2 阶段2a
type Parser interface {
	ToolID() string
	SupportedLanguages() []string
	Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error)
}

// Registry is the global parser registry.
// 依据: 01 §5 十适配器口径（统一注册，一处查找）
var Registry = map[string]Parser{}

// Register adds a parser to the global registry.
func Register(p Parser) {
	id := p.ToolID()
	if id == "" {
		panic("adapter parser must return non-empty ToolID()")
	}
	if _, ok := Registry[id]; ok {
		panic(fmt.Sprintf("adapter already registered: %s", id))
	}
	Registry[id] = p
}

// GetParser returns a parser by tool id.
func GetParser(toolID string) (Parser, error) {
	p, ok := Registry[toolID]
	if !ok {
		return nil, fmt.Errorf("unsupported SAST tool: %s", toolID)
	}
	return p, nil
}

// ListTools returns all registered tool ids.
func ListTools() []string {
	out := make([]string, 0, len(Registry))
	for k := range Registry {
		out = append(out, k)
	}
	return out
}

// BuildToolScanResult is a helper to build normalized result.
func BuildToolScanResult(toolName string, findings []UnifiedFinding, duration time.Duration, scannedFiles int32) ToolScanResult {
	bySeverity := map[string]int32{}
	for _, f := range findings {
		bySeverity[f.Severity]++
	}
	return ToolScanResult{
		ToolName: toolName,
		Findings: findings,
		Metrics: ScanMetrics{
			DurationMs:    duration.Milliseconds(),
			FilesScanned:  scannedFiles,
			FindingsCount: int32(len(findings)),
			BySeverity:    bySeverity,
		},
		Status: "SCAN_STATUS_COMPLETED",
	}
}

// ValidateRequired checks that required unified fields are present.
// 依据: codeaudit_common.proto L55-L66（必要字段）
func ValidateRequired(f UnifiedFinding) error {
	if f.SourceTool == "" {
		return errors.New("source_tool is required")
	}
	if f.Location.FilePath == "" {
		return errors.New("location.file_path is required")
	}
	if f.Location.StartLine <= 0 {
		return errors.New("location.start_line must be > 0")
	}
	return nil
}

// SeverityToProto maps adapter raw severity strings to proto enum name.
// 依据: codeaudit_common.proto L120-L127
func SeverityToProto(raw string) string {
	switch raw {
	case "critical", "CRITICAL", "fatal", "FATAL":
		return "SEVERITY_CRITICAL"
	case "high", "HIGH", "error", "ERROR":
		return "SEVERITY_HIGH"
	case "medium", "MEDIUM", "warning", "WARNING", "moderate", "MODERATE":
		return "SEVERITY_MEDIUM"
	case "low", "LOW":
		return "SEVERITY_LOW"
	case "info", "INFO", "note", "NOTE":
		return "SEVERITY_INFO"
	default:
		return "SEVERITY_MEDIUM"
	}
}

// ConfidenceToProto normalizes tool-specific confidence strings.
func ConfidenceToProto(raw string) float32 {
	switch raw {
	case "high", "HIGH", "very-high", "VERY_HIGH", "Very High":
		return 0.9
	case "medium", "MEDIUM", "moderate", "MODERATE", "Medium":
		return 0.7
	case "low", "LOW", "Low":
		return 0.5
	default:
		return 0.6
	}
}

// FileContext — 匹配点上下文窗口（ADR-143）：人工复核与 LLM 审核都需要"看见周围的代码"，
// 仅匹配行不足以判断。窗口 ±ContextRadius 行，文件不可读时返回 ok=false（诚实降级，
// 不伪造上下文）。
type FileContext struct {
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Lines     []string `json:"lines"`
}

const ContextRadius = 10

// FileContextAt — 读取 path 的 [line-R, line+R] 行窗口（1-based；行号越界自动收敛）。
func FileContextAt(path string, line int, radius int) (FileContext, bool) {
	if line <= 0 || radius <= 0 {
		return FileContext{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FileContext{}, false
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	start := line - radius
	if start < 1 {
		start = 1
	}
	end := line + radius
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return FileContext{}, false
	}
	return FileContext{StartLine: start, EndLine: end, Lines: lines[start-1 : end]}, true
}

// ContextJSON — FileContext 序列化为 SourceRaw 的 context 字段（空窗口时省略）。
func ContextJSON(fc FileContext) json.RawMessage {
	if len(fc.Lines) == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]interface{}{
		"start_line": fc.StartLine,
		"end_line":   fc.EndLine,
		"lines":      fc.Lines,
	})
	if err != nil {
		return nil
	}
	return b
}
