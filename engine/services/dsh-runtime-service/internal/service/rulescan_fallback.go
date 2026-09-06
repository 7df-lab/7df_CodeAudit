// 沙箱 DSH 不可用时的内置 RuleScan 兜底（ADR-175，人类裁决 2026-09-01）：
// ai-inference 服务删除后，降级终点本地化（07 §10 链尾从"gRPC 调 ai-inference"
// 收敛为进程内引擎调用）。
//
// 人类硬性要求：降级产出必须显式标注"是降级"——三重标注缺一不可：
//   1. ai_verdict=NEEDS_MANUAL + ai_reasoning 声明（结论侧：绝非 AI 语义判定）；
//   2. source_rule_id 前缀 rulescan-fallback:（机器可辨来源）；
//   3. description 前缀（GUI 人读可见）。
// 同时执行日志出 warn 级降级事件（原因+口径），不留静默降级。
package service

import (
	"fmt"
	"log"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"

	"github.com/codeaudit/services/dsh-runtime-service/internal/rulescan"
)

var (
	rulescanOnce sync.Once
	rulescanEng  *rulescan.Engine
)

func rulescanEngine() *rulescan.Engine {
	rulescanOnce.Do(func() { rulescanEng = rulescan.NewEngine() })
	return rulescanEng
}

// rulescanFallback — 沙箱 DSH 不可用 → 内置规则引擎兜底（显式降级标注）。
// 返回发现已带三重降级标注；引擎故障（如路径不可读）返回空集并 log 如实（任务不失败，04 §6）。
func rulescanFallback(taskID, projectPath, reason string, emit TaskLogFunc) []*pb.UnifiedFinding {
	emit("warn", fmt.Sprintf("沙箱 DSH 不可用（%s）→ 已降级为内置规则引擎 RuleScan 兜底：产出为规则匹配（非 AI 语义分析），全部标注 NEEDS_MANUAL 待人工复核", reason))
	log.Printf("[dsh-runtime][%s] DEGRADED to local RuleScan (sandbox unavailable: %s)", taskID, reason)

	start := time.Now()
	findings, err := rulescanEngine().ScanDirectory(projectPath, nil)
	if err != nil {
		// 04 §6/07 §10：规则引擎亦不可用 → 空产出返回（任务不失败），如实留痕
		log.Printf("[dsh-runtime][%s] local RuleScan failed (%v), empty result", taskID, err)
		emit("error", fmt.Sprintf("内置 RuleScan 亦不可用（%v），空产出返回（07 §10 降级链终点）", err))
		return []*pb.UnifiedFinding{}
	}
	for i, f := range findings {
		f.FindingId = fmt.Sprintf("%s-rsfallback-%d", taskID, i+1)
		f.TaskId = taskID
		f.SourceTool = "ai_agent" // 流程位置口径（04 §3.2 4b 阶段产物）；真实来源见 source_rule_id 前缀
		f.SourceRuleId = "rulescan-fallback:" + f.GetSourceRuleId()
		f.Description = "[降级·RuleScan] 沙箱 DSH 不可用，本条由内置规则引擎匹配产出（非 AI 语义分析）: " + f.GetDescription()
		f.AiVerdict = pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL
		f.AiConfidence = 0.3
		f.AiReasoning = "[降级] 规则引擎兜底产出，未经 AI 语义审查，需人工复核（07 §10）"
		f.Confidence = 0.3
	}
	log.Printf("[dsh-runtime][%s] local RuleScan: %d findings in %dms (all marked NEEDS_MANUAL)",
		taskID, len(findings), time.Since(start).Milliseconds())
	emit("info", fmt.Sprintf("内置 RuleScan 产出 %d 条（已全部标注降级 NEEDS_MANUAL）", len(findings)))
	return findings
}
