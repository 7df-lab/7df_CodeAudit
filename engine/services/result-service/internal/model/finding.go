package model

import "time"

// 依据: codeaudit_common.proto L920-L935 ResultService
type Finding struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	ToolName   string `json:"tool_name"`
	RuleID     string `json:"rule_id"`
	CWE        string `json:"cwe"`   // 依据: proto L58 cwe_id（模型层补齐，round-trip 保真）
	Title      string `json:"title"` // 依据: proto UnifiedFinding.title（v0.1.2 持久化补齐——此前表无此列落库即丢）
	Severity   string `json:"severity"`
	Message    string `json:"message"`
	FilePath   string `json:"file_path"`
	LineNumber int    `json:"line_number"`
	// ADR-201 正名: 原 CodeSnippet/code_snippet（ADR-141 曾复用该列承载 source_raw），
	// 与 proto L67 source_raw 同名直存——该列内容是全量原始输出 JSON，不是代码片段
	SourceRaw string `json:"source_raw"`
	// 判定两轴：原单字段 Verdict 承载七值
	// 压平枚举（真伪/档位/处置三维度混装）拆为方向×档位；存 pb 枚举名字面量，
	// 空/未知串解析回零值（未判定）。派生谓词单源=libs/common-go/verdict。
	VerdictDirection string `json:"verdict_direction"` // VERDICT_DIRECTION_UNSPECIFIED/TRUE/FALSE
	VerdictTier      string `json:"verdict_tier"`      // VERDICT_TIER_UNSPECIFIED/DEFINITE/LIKELY（方向未判定时恒 UNSPECIFIED）
	Reasoning        string `json:"reasoning"`         // AI 判定理由（UpdateVerdict.reasoning, proto L1240；ADR-135 补链路）
	// ADR-183: 修复建议两通道（此前 ai_fix_suggestion 在落盘层被丢弃——插件两代兼容的降级通道一并打通）
	AiFixSuggestion string `json:"ai_fix_suggestion"` // proto UnifiedFinding.ai_fix_suggestion（人类可读 markdown）
	DiffPatch       string `json:"diff_patch"`        // proto UnifiedFinding.diff_patch（apply_patch 语法，写入前已经 runtime 服务端校验重建）
	// 融合字段（proto L84-L86；ADR-142 持久化——此前融合输出从不落盘，融合视图无内容）
	DedupGroup      string    `json:"dedup_group"`
	MatchedFindings string    `json:"matched_findings"` // JSON 数组字符串
	IsUnique        bool      `json:"is_unique"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	RequestID       string    `json:"request_id"` // 幂等键 - 依据: 03 §2
	// ADR-225 增量扫描继承标记：空=本任务实扫产出；非空=从该基线任务复制的继承项
	//（继承行连带 verdict/AI 建议终态复制；复制不是引用，本任务上的变更不回写基线）
	InheritedFrom string `json:"inherited_from_task_id"`
}

// 依据: codeaudit_common.proto L1246-L1253 FindingFeedback
type FindingFeedback struct {
	ID           string    `json:"id"`
	FindingID    string    `json:"finding_id"`
	FeedbackType string    `json:"feedback_type"` // 依据: L1249 FeedbackType enum
	Comment      string    `json:"comment"`
	CreatedAt    time.Time `json:"created_at"`
	RequestID    string    `json:"request_id"` // 幂等键 - 依据: 03 §2
}

// 依据: 03 §5 cursor 分页
type Cursor struct {
	LastID string `json:"last_id"`
	Limit  int    `json:"limit"`
}

// 依据: codeaudit_common.proto L1242 ResultStats
type ResultStats struct {
	TaskId        string           `json:"task_id"`
	TotalFindings int              `json:"total_findings"`
	BySeverity    map[string]int32 `json:"by_severity"`
	ByCwe         map[string]int32 `json:"by_cwe"`
	ByVerdict     map[string]int32 `json:"by_verdict"`
}
