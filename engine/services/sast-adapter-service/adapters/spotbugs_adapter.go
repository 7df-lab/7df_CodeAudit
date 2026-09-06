// SpotBugs adapter parser (XML output).
// 依据: 01 §5 SpotBugs 口径 + codeaudit_common.proto L52-L94
package adapters

import (
	"encoding/xml"
	"fmt"
	"time"
)

type bugCollection struct {
	XMLName xml.Name      `xml:"BugCollection"`
	Bugs    []bugInstance `xml:"BugInstance"`
}

type bugInstance struct {
	Type     string   `xml:"type,attr"`
	Priority int      `xml:"priority,attr"`
	Rank     int      `xml:"rank,attr"`
	ShortMsg string   `xml:"ShortMessage"`
	LongMsg  string   `xml:"LongMessage"`
	Class    bugClass `xml:"Class"`
}

type bugClass struct {
	ClassName  string     `xml:"classname,attr"`
	SourceLine sourceLine `xml:"SourceLine"`
}

type sourceLine struct {
	ClassName  string `xml:"classname,attr"`
	Start      int    `xml:"start,attr"`
	End        int    `xml:"end,attr"`
	SourceFile string `xml:"sourcefile,attr"`
	SourcePath string `xml:"sourcepath,attr"`
}

type SpotBugsAdapter struct{}

func init() { Register(SpotBugsAdapter{}) }

func (SpotBugsAdapter) ToolID() string               { return "spotbugs" }
func (SpotBugsAdapter) SupportedLanguages() []string { return []string{"java"} }

func (SpotBugsAdapter) Parse(taskID string, projectID string, raw []byte) (ToolScanResult, error) {
	if len(raw) == 0 {
		return ToolScanResult{}, fmt.Errorf("empty spotbugs output")
	}
	var out bugCollection
	if err := xml.Unmarshal(raw, &out); err != nil {
		return ToolScanResult{}, fmt.Errorf("invalid spotbugs xml: %w", err)
	}
	start := time.Now()
	findings := make([]UnifiedFinding, 0, len(out.Bugs))
	for i, b := range out.Bugs {
		severity := "SEVERITY_MEDIUM"
		switch b.Priority {
		case 1:
			severity = "SEVERITY_CRITICAL"
		case 2:
			severity = "SEVERITY_HIGH"
		case 3:
			severity = "SEVERITY_MEDIUM"
		default:
			severity = "SEVERITY_LOW"
		}
		filePath := b.Class.SourceLine.SourcePath
		if filePath == "" {
			filePath = b.Class.SourceLine.SourceFile
		}
		findings = append(findings, UnifiedFinding{
			FindingID:    fmt.Sprintf("%s-spotbugs-%d", taskID, i+1),
			TaskID:       taskID,
			ProjectID:    projectID,
			SourceTool:   "spotbugs",
			SourceRuleID: b.Type,
			SourceRaw:    raw,
			Location: ScanLocation{
				FilePath:  filePath,
				StartLine: b.Class.SourceLine.Start,
				EndLine:   b.Class.SourceLine.End,
			},
			Title:       b.ShortMsg,
			Description: b.LongMsg,
			Severity:    severity,
			RawSeverity: fmt.Sprintf("priority-%d", b.Priority),
			Confidence:  0.75,
		})
	}
	return BuildToolScanResult("spotbugs", findings, time.Since(start), 1), nil
}
