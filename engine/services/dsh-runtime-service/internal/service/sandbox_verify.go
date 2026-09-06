// 4a/4b 沙箱内审查（ADR-173，人类指令 2026-09-01）：
//   - 4a：整项目上传沙箱磁盘，逐条发现一个 prompt 交 DSH 自行阅读完整源码判定
//     （数据流/净化/可达性都在项目全文上核对，不再依赖报告行±5 的本地摘录），
//     逐条取回最终结论，全部完成后再销毁沙箱；
//   - 4b：整项目上传后单 prompt 全项目审计，取回发现 JSON。
// AI 交互（SSE）经 Config.OnHumanLog/OnRawLog 双写 AI 交互日志（内存=.ai.log 落盘），
// 因此模式B 的增强自此在 GUI 有实时交互日志（此前 ai-inference 直连无日志）。
package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"log"
	"time"

	pb "github.com/codeaudit/proto-gen"

	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

// newSandboxRunner — 组装带日志双流出口的沙箱执行器（生命周期事件→执行日志；
// 人性化流→AI 交互日志；原始帧→.sse.log）。返回的 entry 由调用方 defer finish。
func newSandboxRunner(taskID string, emit TaskLogFunc) (*sandbox.ManagerRunner, *aiLogEntry, error) {
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, nil, err
	}
	if emit != nil {
		cfg.EventFn = func(level, msg string) { emit(level, msg) } // source=sandbox 由 taskLogSink 标定
	}
	aiEntry := sharedAILogs.writer(taskID)
	cfg.OnHumanLog = func(s string) { aiEntry.write([]byte(s)) }
	cfg.OnRawLog = aiEntry.writeRaw
	return sandbox.NewManagerRunner(*cfg), aiEntry, nil
}

// sandboxRelPath — 发现里的文件路径 → 沙箱内项目相对路径（绝对路径落在项目目录下时取相对；
// 否则原样交 DSH 自行定位）。
func sandboxRelPath(projectPath, filePath string) string {
	if projectPath == "" || filePath == "" {
		return filePath
	}
	if rel, err := filepath.Rel(projectPath, filePath); err == nil &&
		!strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filePath)
}

// verifyViaSandbox — 4a 沙箱通道（ADR-173）：先同文件同段去重（ADR-186，一组一轮
// 沙箱、判定广播），逐组 prompt、逐组取回最终结论，全部完成后由 RunSession 统一
// 销毁沙箱。任一回合失败即整体报错（调用方回退直连降级）。
func verifyViaSandbox(ctx context.Context, taskID, projectPath string,
	findings []*pb.UnifiedFinding, emit TaskLogFunc) ([]*pb.VerifiedFinding, error) {
	if len(findings) == 0 {
		return []*pb.VerifiedFinding{}, nil
	}
	r, aiEntry, err := newSandboxRunner(taskID, emit)
	if err != nil {
		return nil, err
	}
	defer aiEntry.finish()
	if !r.Enabled() {
		return nil, sandbox.ErrDisabled
	}

	// ADR-186：进沙箱前同段去重——同文件同段只发一轮，跨工具重复不再重复进沙箱。
	groups := groupSegments(findings)
	if len(groups) < len(findings) && emit != nil {
		emit("info", fmt.Sprintf("沙箱验证同段去重：%d 条发现 → %d 轮（省 %d 轮，容差±%d 行）",
			len(findings), len(groups), len(findings)-len(groups), segmentRadiusLines))
	}

	prompts := make([]string, 0, len(groups))
	for gi, g := range groups {
		prompts = append(prompts, fmt.Sprintf(verifyTurnPrompt,
			sandbox.ProjectSandboxPath, gi+1, len(groups),
			sandboxRelPath(projectPath, g.Rep.GetLocation().GetFilePath()),
			g.Rep.GetLocation().GetStartLine(),
			g.Rep.GetSourceRuleId(), g.Rep.GetCweId(),
			g.Rep.GetTitle(), g.Rep.GetDescription(),
			segmentPeersNote(g)))
	}
	finals, err := r.RunSession(ctx, sandbox.SessionTask{
		TaskID:     taskID,
		ProjectDir: projectPath,
		Prompts:    prompts,
		Timeout:    30 * time.Minute, // 07 §8 OpenShell 沙箱执行 30m
	})
	if err != nil {
		return nil, err
	}

	// 每组解析一次，判定广播回组内全部成员（契约：每输入 finding_id 恰一条）。
	verified := make([]*pb.VerifiedFinding, 0, len(findings))
	for gi, g := range groups {
		base := parseVerdictTurn(finals[gi], g.Rep)
		verified = append(verified, broadcastGroupVerdict(base, g.Rep, g.Members)...)
	}
	return verified, nil
}

// segmentPeersNote — 组内除代表外的关联发现附注（DSH 看到全部工具主张，判定更准）。
func segmentPeersNote(g segmentGroup) string {
	if len(g.Members) <= 1 {
		return ""
	}
	parts := make([]string, 0, len(g.Members)-1)
	for _, m := range g.Members {
		if m.GetFindingId() == g.Rep.GetFindingId() {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%s", m.GetSourceTool(), m.GetSourceRuleId()))
	}
	return fmt.Sprintf("\n注意：另有 %d 条工具发现与上述发现指向同文件同段代码（%s），请在本次判定中一并核对。",
		len(parts), strings.Join(parts, "、"))
}

// discoverViaSandbox — 4b 沙箱通道（ADR-173）：单 prompt 全项目审计，结果经
// ParseFindings 解析并映射 UnifiedFinding（dedup 已知项；落盘由调用方承接）。
func discoverViaSandbox(ctx context.Context, taskID, projectPath string,
	knownKeys map[string]bool, emit TaskLogFunc) ([]*pb.UnifiedFinding, error) {
	r, aiEntry, err := newSandboxRunner(taskID, emit)
	if err != nil {
		return nil, err
	}
	defer aiEntry.finish()
	if !r.Enabled() {
		return nil, sandbox.ErrDisabled
	}

	finals, err := r.RunSession(ctx, sandbox.SessionTask{
		TaskID:     taskID,
		ProjectDir: projectPath,
		Prompts:    []string{fmt.Sprintf(discoverTurnPrompt, sandbox.ProjectSandboxPath)},
		Timeout:    30 * time.Minute, // 07 §8 OpenShell 沙箱执行 30m
	})
	if err != nil {
		return nil, err
	}
	findings, perr := sandbox.ParseFindings(finals[0])
	if perr != nil {
		return nil, fmt.Errorf("dsh audit result parse: %w", perr)
	}

	out := make([]*pb.UnifiedFinding, 0, len(findings))
	n := 0
	for _, f := range findings {
		cand := &pb.UnifiedFinding{
			FindingId:    fmt.Sprintf("%s-sbxm-%d", taskID, n+1),
			TaskId:       taskID,
			SourceTool:   "ai_agent", // 04 §3.2 4b 口径
			SourceRuleId: "dsh-sandbox",
			Title:        f.Title,
			Description:  f.Description,
			Severity:     pb.Severity(pb.Severity_value[f.Severity]),
			CweId:        f.CweID,
			Confidence:   0.5, // 未经二次验证的 AI 候选——低置信诚实标注
			AiVerdict:    pb.AIVerdict_AI_VERDICT_LIKELY_TRUE,
			AiReasoning:  "[DSH-sandbox] " + f.Reasoning,
			Location:     &pb.LocationInfo{FilePath: f.FilePath, StartLine: int32(f.StartLine)},
			SourceRaw:    captureCodeContext(projectPath, f.FilePath, f.StartLine),
		}
		if knownKeys[dedupKey(cand)] {
			continue
		}
		n++
		cand.FindingId = fmt.Sprintf("%s-sbxm-%d", taskID, n)
		out = append(out, cand)
	}
	return out, nil
}

// verifyTurnPrompt — 单段审查 prompt（DSH 自行阅读沙箱内完整源码后判定；一个 prompt
// 对应一个同段去重组，ADR-186）。最后一个 %s 为同段关联发现附注（单条发现时为空）。
const verifyTurnPrompt = `你是代码安全审计员。待审计项目的完整源码已放在你所在环境的 %s 目录。

待核实的安全发现（第 %d/%d 组）：
- 文件: %s
- 行号: %d
- 规则: %s (%s)
- 标题: %s
- 描述: %s
%s
请自行阅读目录中的项目源码（不要只看报告行，结合整个项目核对），追踪数据流、
检查输入净化与漏洞触发路径是否真实可达，判断该发现是否为真实漏洞。
只输出一行 JSON，不要任何其他文字:
{"verdict":"TRUE_POSITIVE|FALSE_POSITIVE|UNCERTAIN","confidence":0.0-1.0,"reason":"<一句话中文理由>"}`

// discoverTurnPrompt — 4b 全项目审计 prompt。
const discoverTurnPrompt = `你是资深渗透测试员。待审计项目的完整源码已放在你所在环境的 %s 目录。

请对该项目做整体安全审计，找出真实存在的安全漏洞，包括 SAST 规则难以覆盖的逻辑缺陷、
硬编码凭据、权限缺失、不安全反序列化等。逐文件阅读源码并核对可触发路径。

分析完成后，把最终结论作为一个 JSON 代码块（` + "```json ... ```" + `）输出为你的最后一条消息。
JSON schema:
{
  "findings": [
    {
      "title": "简短漏洞标题",
      "description": "问题与影响说明",
      "severity": "SEVERITY_CRITICAL|SEVERITY_HIGH|SEVERITY_MEDIUM|SEVERITY_LOW|SEVERITY_INFO",
      "cwe_id": "CWE-XXX 或空",
      "file_path": "项目内相对路径",
      "start_line": 行号整数,
      "confidence": 0.0到1.0,
      "reasoning": "判定理由（引用代码证据）"
    }
  ]
}
没有发现时输出 {"findings": []}。`

// parseVerdictTurn — 解析 4a 单回合最终消息为 VerifiedFinding；解析失败/为空时
// 如实降级 NEEDS_MANUAL（不编造判定，2026-08-27 诚实化纪律同源）。
func parseVerdictTurn(final string, f *pb.UnifiedFinding) *pb.VerifiedFinding {
	vf := &pb.VerifiedFinding{
		OriginalFindingId: f.GetFindingId(),
		Verdict:           pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL,
		Confidence:        0.3,
	}
	trimmed := strings.TrimSpace(final)
	if trimmed == "" {
		vf.Reasoning = "DSH 沙箱回合无最终消息，降级为需人工复核（07 §10，未做语义判断）"
		return vf
	}
	verdict := parseVerdictJSON(trimmed)
	if verdict == nil {
		vf.Reasoning = fmt.Sprintf("DSH 回合结论无法解析(raw=%.80s)，保守降级为需人工复核", trimmed)
		return vf
	}
	vf.Verdict = verdict.Verdict
	vf.Confidence = verdict.Confidence
	vf.Reasoning = "[DSH-sandbox] " + verdict.Reason
	return vf
}

// StartSandboxReconciler — ADR-210 孤儿沙箱对账回收（cmd/main.go 接线入口）。
// mode=openshell 才启用；配置读取复用 sandboxCfg()（与请求侧同源，不漂移）。
// 返回是否实际启动（供测试/运维观测）。
func StartSandboxReconciler() bool {
	cfg, err := sandboxCfg()
	if err != nil {
		log.Printf("[sandbox-reconciler] config unavailable, not started: %v", err)
		return false
	}
	if cfg.Mode != "openshell" {
		return false
	}
	sandbox.NewSandboxReconciler(sandbox.NewManagerRunner(*cfg)).Start()
	return true
}
