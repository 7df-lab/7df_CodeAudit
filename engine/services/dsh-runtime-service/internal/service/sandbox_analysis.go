// 沙箱语义分析接线（ADR-140/ADR-166）：模式A RunAIAnalysis 与模式D ReviewSASTResults 的
// LLM 语义环节经 openshell-manager 微服务在沙箱内执行 DSH；不可用（mode=off/manager
// 不可达/执行失败）时回退既有降级链（RuleScan / NEEDS_MANUAL，07 §10），回退原因留日志。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

// sandboxCfg — 从全局配置装配沙箱执行器（缺键 fail-fast，ADR-137）。
func sandboxCfg() (*sandbox.Config, error) {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return nil, err
	}
	mode, err := cfg.Str("dsh_runtime.sandbox.mode")
	if err != nil {
		return nil, err
	}
	mgrURL, err := cfg.Str("dsh_runtime.sandbox.manager_url")
	if err != nil {
		return nil, err
	}
	mgrToken, err := cfg.Str("dsh_runtime.sandbox.manager_token")
	if err != nil {
		return nil, err
	}
	workspace, err := cfg.Str("dsh_runtime.sandbox.workspace")
	if err != nil {
		return nil, err
	}
	image, err := cfg.Str("dsh_runtime.sandbox.image")
	if err != nil {
		return nil, err
	}
	waitS, err := cfg.Int("dsh_runtime.sandbox.wait_ready_timeout_s")
	if err != nil {
		return nil, err
	}
	execS, err := cfg.Int("dsh_runtime.sandbox.exec_timeout_s")
	if err != nil {
		return nil, err
	}
	maxTok, err := cfg.Int("dsh_runtime.sandbox.dsh_max_tokens")
	if err != nil {
		return nil, err
	}
	// env 覆盖（2026-09-05 生产脚本需要：yaml 硬编码现役实例 IP，任意服务器部署时
	// 经 CODEAUDIT_GATEWAY_DIAL_ADDR 注入本机网关拨号地址；空值语义不变=由 manager 推导）
	gatewayDial, err := cfg.Str("dsh_runtime.sandbox.gateway_dial_addr", "CODEAUDIT_GATEWAY_DIAL_ADDR")
	if err != nil {
		return nil, err
	}
	return &sandbox.Config{
		Mode: mode, ManagerURL: mgrURL, ManagerToken: mgrToken,
		Workspace: workspace, Image: image,
		WaitReadyTimeoutS: waitS, ExecTimeoutS: execS,
		DSHMaxTokens: maxTok, GatewayDialAddr: gatewayDial,
	}, nil
}

// interactionDir — AI 交互日志落盘根目录（ADR-168；读失败=空 → 仅内存留存）。
func interactionDir() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return ""
	}
	v, err := cfg.Str("dsh_runtime.sandbox.interaction_dir")
	if err != nil {
		return ""
	}
	return v
}

// analyzeViaSandbox — 沙箱内 DSH 语义分析；返回 (findings, nil) 或 (nil, err)。
// 调用方收到 err 时回退降级链。reasoning 统一加 [DSH-sandbox] 前缀（溯源标记）。
// emit 可空：沙箱生命周期事件出口（GUI 执行日志，ADR-167）。
func analyzeViaSandbox(ctx context.Context, taskID, projectPath, assignment string, emit TaskLogFunc) ([]*pb.UnifiedFinding, error) {
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, err
	}
	if emit != nil {
		cfg.EventFn = func(level, msg string) { emit(level, msg) } // source=sandbox 由 taskLogSink 标定
	}
	// AI 交互日志（ADR-168 补遗）：人性化流→内存（GetAIInteractionLog 面向用户）+ .ai.log
	// 落盘；原始 SSE 帧→仅 .sse.log 落盘（机器调试留存，不经 RPC）。终态定格。落盘失败
	// 静默——留存通道不得影响分析主链路（07 §10 降级纪律同源）。
	aiEntry := sharedAILogs.writer(taskID)
	cfg.OnHumanLog = func(s string) { aiEntry.write([]byte(s)) }
	cfg.OnRawLog = aiEntry.writeRaw
	defer aiEntry.finish()
	r := sandbox.NewManagerRunner(*cfg)
	if !r.Enabled() {
		return nil, sandbox.ErrDisabled
	}
	res, err := r.Run(ctx, sandbox.Task{
		TaskID:       taskID,
		WorkspaceDir: projectPath,
		Assignment:   assignment,
		Timeout:      10 * time.Minute, // 07 §8 单次推理执行 10m 的沙箱映射
	})
	if err != nil {
		return nil, err
	}
	// ADR-183 补遗②：非空但校验失败的补丁走一轮失败反馈再生成（Cline 式自纠，
	// 失败详情含相似度+上下文预览喂回模型）；全部干净/模型未产补丁时零开销。
	// 再生成回合复用同一 runner（每 Run 独立沙箱生命周期）。
	res.Findings = retryFailedPatches(ctx, taskID, projectPath, res.Findings, emit, func(ctx context.Context, fixAssignment string) (string, error) {
		fixRes, fixErr := r.Run(ctx, sandbox.Task{
			TaskID:       taskID + "-fix",
			WorkspaceDir: projectPath,
			Assignment:   fixAssignment,
			Timeout:      10 * time.Minute, // 07 §8 单次推理执行 10m
		})
		if fixErr != nil {
			return "", fixErr
		}
		// ADR-184：submit_patches 工具参数（原生 function-calling）优先；
		// ParsePatches 对裸 JSON 走括号区间提取，参数原文可直接喂入。
		if args, ok := sandbox.LastToolCallArgs(fixRes.ToolCalls, sandbox.SubmitPatchesTool); ok {
			return args, nil
		}
		return fixRes.FinalText, nil
	})
	return mapSandboxFindings(taskID, projectPath, res.Findings), nil
}

// mapSandboxFindings — 沙箱发现 → proto UnifiedFinding。
// AiVerdict=LIKELY_TRUE：DSH 报告即模型断言"这是真实漏洞"，但不越权标 TRUE_POSITIVE
// （那是对齐人工确认的口径；V1 契约 AI/人工共用字段，语义靠 reasoning 前缀区分）。
// SourceRaw：捕获发现位置 ±10 行真实文件内容（对齐 ADR-143 适配器同款 schema，
// GUI 代码上下文卡片直接渲染；读取失败如实留空——不编造上下文）。
// ADR-183：AiFixSuggestion（LLM markdown 原文）+ DiffPatch（经 fixpatch 服务端校验重建，
// 失配丢弃置空——finding 保留，不编造补丁）。
func mapSandboxFindings(taskID, projectPath string, findings []sandbox.Finding) []*pb.UnifiedFinding {
	out := make([]*pb.UnifiedFinding, 0, len(findings))
	for i, f := range findings {
		out = append(out, &pb.UnifiedFinding{
			FindingId:       fmt.Sprintf("%s-sbx-%d", taskID, i+1),
			TaskId:          taskID,
			SourceTool:      "ai_agent", // 04 §3.1 阶段3 产物口径
			SourceRuleId:    "dsh-headless",
			Title:           f.Title,
			Description:     f.Description,
			Severity:        pb.Severity(pb.Severity_value[f.Severity]),
			CweId:           f.CweID,
			Confidence:      float32(f.Confidence),
			AiVerdict:       pb.AIVerdict_AI_VERDICT_LIKELY_TRUE,
			AiConfidence:    float32(f.Confidence),
			AiReasoning:     "[DSH-sandbox] " + f.Reasoning,
			AiFixSuggestion: f.FixSuggestion,
			DiffPatch:       validatedDiffPatch(taskID, f.DiffPatch, projectPath),
			Location:        &pb.LocationInfo{FilePath: f.FilePath, StartLine: int32(f.StartLine)},
			SourceRaw:       captureCodeContext(projectPath, f.FilePath, f.StartLine),
		})
	}
	return out
}

// codeCtxRadius — 代码上下文捕获半径（与 ADR-143 适配器 ±10 行同口径）。
const codeCtxRadius = 10

// captureCodeContext — 从项目文件读取发现位置 ±10 行，产出 ADR-143 source_raw schema
// 的原始 JSON 字节（bytes 字段；base64 由 protojson HTTP 序列化层完成）。
// 文件不可读/越界如实返回空——不编造上下文。
func captureCodeContext(projectPath, filePath string, startLine int) []byte {
	if projectPath == "" || filePath == "" || startLine <= 0 {
		return nil
	}
	abs := filepath.Join(projectPath, filepath.Clean("/"+filePath)) // 清洗防穿越
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if startLine > len(lines) {
		return nil
	}
	s := startLine - codeCtxRadius
	if s < 1 {
		s = 1
	}
	e := startLine + codeCtxRadius
	if e > len(lines) {
		e = len(lines)
	}
	window := make([]string, 0, e-s+1)
	for i := s; i <= e; i++ {
		window = append(window, lines[i-1])
	}
	payload, err := json.Marshal(map[string]any{
		"code": lines[startLine-1],
		"line": startLine,
		"context": map[string]any{
			"start_line": s, "end_line": e, "lines": window,
		},
	})
	if err != nil {
		return nil
	}
	return payload
}

// sandboxAssignmentModeA — 模式A 语义分析任务指令（04 §3.1 阶段3 语义）。
func sandboxAssignmentModeA() string {
	return "对项目做安全审计：识别可被利用的漏洞（注入/鉴权缺失/敏感信息/不安全反序列化等），" +
		"逐条给出代码证据与判定理由；按输出契约返回 findings。"
}

// sandboxAssignmentReview — 模式D 逐条审核任务指令（04 §3.4 步骤2 口径）。
func sandboxAssignmentReview(findingsJSON string) string {
	return "以下是 SAST 工具的发现清单（JSON）。逐条交叉验证（读代码证据→数据流→可达性），" +
		"对每条给出 verdict（TRUE_POSITIVE/FALSE_POSITIVE/NEEDS_MANUAL）与理由；" +
		"同时补充清单遗漏的真实漏洞到 findings。按输出契约返回：" +
		`{"findings": [...], "reviews": [{"finding_id":"...","verdict":"...","reasoning":"..."}]}` +
		"\n\n发现清单：\n" + findingsJSON
}

// sandboxErrHint — 降级日志用：错误摘要（去换行）。
func sandboxErrHint(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", " ")
}

// sandboxReview — 模式D 沙箱逐条审核：一次沙箱运行完成全部交叉验证。
// 返回 (reviews, opinionCount, err)；ErrDisabled/失败由调用方回退降级链。
func sandboxReview(ctx context.Context, taskID string, findings []*pb.UnifiedFinding, emit TaskLogFunc) (
	[]*pb.FindingReview, map[string]int32, error) {
	if len(findings) == 0 {
		return nil, nil, fmt.Errorf("no findings to review")
	}
	list := make([]map[string]interface{}, 0, len(findings))
	for _, f := range findings {
		list = append(list, map[string]interface{}{
			"finding_id": f.GetFindingId(), "title": f.GetTitle(), "cwe_id": f.GetCweId(),
			"severity": f.GetSeverity().String(), "file": f.GetLocation().GetFilePath(),
			"line": f.GetLocation().GetStartLine(),
		})
	}
	raw, _ := json.Marshal(list)
	res, err := analyzeViaSandbox(ctx, taskID, os.Getenv("CODEAUDIT_PROJECT_REPO_PATH"),
		sandboxAssignmentReview(string(raw)), emit)
	if err != nil {
		return nil, nil, err
	}
	// 从沙箱原始 stdout 拿 reviews（analyzeViaSandbox 只映射了 findings）——
	// 简化：reviews 由 runner envelope 无法承载，故此处要求沙箱输出 reviews 字段时
	// 通过 ai_reasoning 回填 verdict 映射（reviews 由 [DSH-sandbox] 前缀 reasoning 的
	// findings 与原清单 dedupKey 匹配生成）。
	reviews := make([]*pb.FindingReview, 0, len(findings))
	opinions := map[string]int32{}
	byKey := map[string]*pb.UnifiedFinding{}
	for _, f := range res {
		byKey[dedupKey(f)] = f
	}
	for _, f := range findings {
		verdict := pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL
		if m, ok := byKey[dedupKey(f)]; ok && m.GetAiConfidence() > 0 {
			verdict = m.GetAiVerdict()
		}
		opinion := pb.ReviewOpinion_REVIEW_OPINION_SUGGEST_REVIEW
		switch verdict {
		case pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE, pb.AIVerdict_AI_VERDICT_LIKELY_TRUE:
			opinion = pb.ReviewOpinion_REVIEW_OPINION_CONFIRM
		case pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE, pb.AIVerdict_AI_VERDICT_LIKELY_FALSE:
			opinion = pb.ReviewOpinion_REVIEW_OPINION_REJECT
		}
		opinions[opinion.String()]++
		reasoning := "sandbox DSH cross-validation unavailable for this item"
		if m, ok := byKey[dedupKey(f)]; ok && m.GetAiReasoning() != "" {
			reasoning = m.GetAiReasoning()
		}
		reviews = append(reviews, &pb.FindingReview{
			FindingId:  f.GetFindingId(),
			Opinion:    opinion,
			Confidence: 0.5,
			Reasoning:  reasoning,
		})
	}
	return reviews, opinions, nil
}
