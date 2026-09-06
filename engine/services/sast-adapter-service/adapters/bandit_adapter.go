// Bandit adapter parser (JSON output).
// 依据: 01 §5 Bandit 口径 + codeaudit_common.proto L52-L94
package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

type banditResult struct {
	TestID        string `json:"test_id"`
	TestName      string `json:"test_name"`
	Filename      string `json:"filename"`
	LineNumber    int    `json:"line_number"`
	IssueText     string `json:"issue_text"`
	IssueSeverity string `json:"issue_severity"`
	IssueConf     string `json:"issue_confidence"`
	MoreInfo      string `json:"more_info"`
	Code          string `json:"code"`
	IssueCwe      *struct {
		ID   int    `json:"id"`
		Link string `json:"link"`
	} `json:"issue_cwe"`
}

type banditOutput struct {
	Results []banditResult `json:"results"`
}

type BanditAdapter struct{}

func init() { Register(BanditAdapter{}) }

func (BanditAdapter) ToolID() string               { return "bandit" }
func (BanditAdapter) SupportedLanguages() []string { return []string{"python"} }

func (BanditAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, fmt.Errorf("empty bandit output")
	}
	var out banditOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid bandit json: %w", err)
	}
	start := time.Now()
	findings := make([]UnifiedFinding, 0, len(out.Results))
	for i, r := range out.Results {
		// CWE 提取: bandit>=1.7 原生 issue_cwe 字段优先；回退 MoreInfo 链接里的 cwe id
		cwe := ""
		if r.IssueCwe != nil && r.IssueCwe.ID > 0 {
			cwe = fmt.Sprintf("CWE-%d", r.IssueCwe.ID)
		} else if idx := indexCWE(r.MoreInfo); idx != "" {
			cwe = idx
		}
		// ADR-141/142: per-finding 代码上下文（此前塞整份 run 输出：体积倍增+UI/报告无法提取代码行）
		// ADR-143: 匹配点 ±10 行窗口（人工复核需"看见周围的代码"；文件不可读时诚实省略）
		payload := map[string]interface{}{
			"tool": "bandit", "code": r.Code, "line": r.LineNumber,
			"file": r.Filename, "rule": r.TestID,
		}
		if fc, ok := FileContextAt(r.Filename, r.LineNumber, ContextRadius); ok {
			payload["context"] = json.RawMessage(ContextJSON(fc))
		}
		perFinding, _ := json.Marshal(payload)
		findings = append(findings, UnifiedFinding{
			FindingID:    fmt.Sprintf("%s-bandit-%d", taskID, i+1),
			TaskID:       taskID,
			ProjectID:    projectID,
			SourceTool:   "bandit",
			SourceRuleID: r.TestID,
			SourceRaw:    perFinding,
			Location: ScanLocation{
				FilePath:  r.Filename,
				StartLine: r.LineNumber,
				EndLine:   r.LineNumber,
			},
			CWEID:         cwe,
			Title:         r.IssueText,
			Description:   r.IssueText,
			Severity:      SeverityToProto(r.IssueSeverity),
			RawSeverity:   r.IssueSeverity,
			Confidence:    ConfidenceToProto(r.IssueConf),
			RawConfidence: r.IssueConf,
			RawLines:      r.Code,
			RawMoreInfo:   r.MoreInfo,
		})
	}
	return BuildToolScanResult("bandit", findings, time.Since(start), 1), nil
}

// indexCWE — 从 MoreInfo 链接提取 CWE 编号（旧版 bandit 兼容回退）。
func indexCWE(moreInfo string) string {
	for i := 0; i+3 < len(moreInfo); i++ {
		if moreInfo[i:i+4] == "CWE-" {
			j := i + 4
			for j < len(moreInfo) && moreInfo[j] >= '0' && moreInfo[j] <= '9' {
				j++
			}
			if j > i+4 {
				return moreInfo[i:j]
			}
		}
	}
	return ""
}
