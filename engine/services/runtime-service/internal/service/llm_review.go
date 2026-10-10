// 判定解析（ADR-175 瘦身）：ai-inference 服务已删除，本文件此前的直连审查链
// （dialReviewLLM/llmReviewWith/aiNativeDiscovery/readSnippet 等，自 ADR-140 起拿到
// 的只有 [RuleScan Fallback] 占位）整段移除；仅保留 DSH 沙箱回合最终消息的判定
// JSON 解析（sandbox_verify.go 复用）。
package service

import (
	"encoding/json"
	"strings"

	pb "github.com/codeaudit/proto-gen"
)

// verdictJSON — 逐条审查判定的中立投影（来源可以是 ai-inference 直连或 DSH 沙箱回合）。
type verdictJSON struct {
	Direction  pb.VerdictDirection // 结构化判定协议：识别即 DEFINITE 档（两轴重构 2026-10-09）
	Confidence float32
	Reason     string
}

// parseVerdictJSON — 判定 JSON 的通用解析（ai-inference 直连与 DSH 沙箱回合同构，
// ADR-173）；无法解析返回 nil。未识别 verdict 字面量保守落 UNCERTAIN。
func parseVerdictJSON(content string) *verdictJSON {
	var raw struct {
		Verdict    string  `json:"verdict"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &raw); err != nil {
		// 容忍 markdown 代码栅栏包裹
		if i := strings.Index(content, "{"); i >= 0 {
			if j := strings.LastIndex(content, "}"); j > i {
				if err := json.Unmarshal([]byte(content[i:j+1]), &raw); err != nil {
					return nil
				}
			} else {
				return nil
			}
		} else {
			return nil
		}
	}
	// 未识别字面量（含 NEEDS_MANUAL/UNCERTAIN 等）保守落未判定——"为什么没结论"
	// 属 provenance（Reason 保留模型原文），不占判定位
	v := &verdictJSON{
		Direction:  pb.VerdictDirection_VERDICT_DIRECTION_UNSPECIFIED,
		Confidence: float32(raw.Confidence),
		Reason:     raw.Reason,
	}
	switch strings.ToUpper(raw.Verdict) {
	case "TRUE_POSITIVE":
		v.Direction = pb.VerdictDirection_VERDICT_DIRECTION_TRUE
	case "FALSE_POSITIVE":
		v.Direction = pb.VerdictDirection_VERDICT_DIRECTION_FALSE
	}
	return v
}
