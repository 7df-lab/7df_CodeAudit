// Package service — AI 分析引擎（模式A/B/C 的推理侧落地）。
//
// 依据:
//   - 05 §4 五角色流水（Code Analyst → Vuln Detector → Severity Assessor → Fix Advisor →
//     Quality Validator）；LLM 语义分析走沙箱内 headless DSH（ADR-140/173），不可用降级
//     dsh-runtime 进程内 RuleScan 并显式标注降级（ADR-175；原 ai-inference 直连链已删，03 §1.6 已删节）
//   - 07 §8 Task→DSHRuntime 30m（ADR-175: DSH→AIInference 链已删，推理在沙箱内）
//   - 04 §3.2 阶段4a/4b/4c 的语义映射
//   - 09 §2 行 dsh-runtime→result：BatchCreateFindings 落盘
package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// aiConfig — 下游地址与限额。
type aiConfig struct {
	resultAddr string // ADR-117 result=50058
	scanMode   pb.ScanMode
}

func dial(addr string) (*grpc.ClientConn, func(), error) {
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, func() { _ = conn.Close() }, nil
}

// runFiveAgentPipeline — 五角色顺序执行，返回 AI 新发现与验证结论。
//
// 实现说明（E1 溯源）：
//   - Code Analyst: project_path 文件遍历统计（AST 级，CPG 失败的 AST 降级产物——04 §6）
//   - Vuln Detector: LLM(Chat) 不可用→RuleScan 规则引擎产出 AI findings（07 §10）
//   - Severity Assessor: 以规则引擎给出的 severity 为基线（KG 精细化 M9 前占位）
//   - Fix Advisor: 对确认项给出修复建议文本（fix_suggestions 出口）
//   - Quality Validator: 对 RuleScan 行误报模式（测试文件等）打 FALSE_POSITIVE 结论
func (s *DSHRuntimeServiceImpl) runFiveAgentPipeline(ctx context.Context, req *pb.RunAIAnalysisRequest, sessID string) (*pb.RunAIAnalysisResponse, error) {
	start := time.Now()
	cfg := aiConfig{
		resultAddr: cfgAddr("result", "CODEAUDIT_RESULT_ADDR"), // ADR-137
	}

	// ---- Code Analyst: 结构分析（04 §3.1 阶段2 的同源产物复用） ----
	var files int32 = 0
	var totalLines int32 = 0
	_ = filepathWalkLite(req.GetProjectPath(), func(p string, lines int32, lang string) bool {
		files++
		totalLines += lines
		return true
	})
	codeStart := time.Now()
	log.Printf("[dsh-runtime][%s] Code Analyst: files=%d lines=%d", req.GetTaskId(), files, totalLines)
	s.completeAgentPhase(req.GetTaskId(), agent.AgentCodeAnalyst)

	aiFindings := []*pb.UnifiedFinding{}
	fixSuggestions := []*pb.FixSuggestion{}
	fallbackUsed := false
	codeMs := time.Since(codeStart).Milliseconds()

	// ---- Vuln Detector: 优先 OpenShell 沙箱内 DSH（ADR-140/166）；不可用 → AI 原生 → RuleScan 降级链（07 §10） ----
	sbxEmit := taskLogSink(req.GetTaskId(), "sandbox")
	sbxFindings, sbxErr := analyzeViaSandbox(ctx, req.GetTaskId(), req.GetProjectPath(), sandboxAssignmentModeA(), sbxEmit)
	if sbxErr != nil {
		log.Printf("[dsh-runtime][%s] sandbox path unavailable (%s), falling back to RuleScan chain",
			req.GetTaskId(), sandboxErrHint(sbxErr))
		emitTaskLog(req.GetTaskId(), "warn", "dsh-runtime",
			fmt.Sprintf("沙箱路径不可用（%s），降级后续链路（07 §10）", sandboxErrHint(sbxErr)))
	} else {
		aiFindings = append(aiFindings, sbxFindings...)
		log.Printf("[dsh-runtime][%s] sandbox DSH findings=%d", req.GetTaskId(), len(sbxFindings))
		zeroNote := ""
		if len(sbxFindings) == 0 {
			zeroNote = "（干净零发现——完整审计后无漏洞，有效产出，不触发 RuleScan 兜底）"
		}
		emitTaskLog(req.GetTaskId(), "info", "dsh-runtime",
			fmt.Sprintf("沙箱 DSH 产出 %d 项发现%s", len(sbxFindings), zeroNote))
	}
	// ADR-175: ai-inference 删除——AI 原生直连段（dialReviewLLM+aiNativeDiscovery，自 ADR-140
	// 起拿到的只有 fallback 占位）整段移除；沙箱未产出 → 直接内置 RuleScan 兜底
	//（本地引擎，显式降级标注三重：NEEDS_MANUAL + rulescan-fallback: 前缀 + 描述声明）。
	// 口径收窄（gw-5a96f1f7）：RuleScan 只补"沙箱通道失败/不可用"（sbxErr != nil）；
	// 干净零发现（沙箱完整审计、submit_findings 空列表、err==nil）是诚实结论，
	// 触发兜底会把健康审计误标为降级链产物。
	llmMs := int64(0)
	fallbackUsed = sbxErr != nil
	if fallbackUsed && req.GetProjectPath() != "" {
		rsFindings := rulescanFallback(req.GetTaskId(), req.GetProjectPath(),
			sandboxErrHint(sbxErr), taskLogSink(req.GetTaskId(), "dsh-runtime"))
		for _, f := range rsFindings {
			aiFindings = append(aiFindings, f)
		}
		fallbackUsed = len(aiFindings) == 0
		emitTaskLog(req.GetTaskId(), "warn", "dsh-runtime",
			fmt.Sprintf("RuleScan 兜底产出 %d 项发现（已三重标注降级：NEEDS_MANUAL / rulescan-fallback 前缀 / 描述声明，07 §10）", len(aiFindings)))
	}

	// ---- Severity Assessor + Fix Advisor（fix_suggestions 出口） ----
	s.completeAgentPhase(req.GetTaskId(), agent.AgentVulnDetector)
	for _, f := range aiFindings {
		if f.GetSeverity() == pb.Severity_SEVERITY_UNSPECIFIED {
			f.Severity = pb.Severity_SEVERITY_MEDIUM
		}
		fixSuggestions = append(fixSuggestions, buildFixSuggestion(f))
	}

	s.completeAgentPhase(req.GetTaskId(), agent.AgentSeverityAssessor)

	s.completeAgentPhase(req.GetTaskId(), agent.AgentFixAdvisor)

	// ---- Quality Validator ----
	// ADR-134 修复: 测试/示例/样例路径的命中此前被自动判 FALSE_POSITIVE(0.55)——
	// 测试代码中的真实漏洞会被静默降级。无 LLM 交叉验证时改判 NEEDS_MANUAL（人工复核），
	// 不再自动认定为误报。依据: 05 §4 Quality Validator 语义（交叉验证而非路径黑名单）。
	fp := 0
	suspicious := 0
	for _, f := range aiFindings {
		p := strings.ToLower(f.GetLocation().GetFilePath())
		if strings.Contains(p, "_test") || strings.Contains(p, "/test") || strings.Contains(p, "sample") || strings.Contains(p, "mock") {
			f.AiVerdict = pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL
			f.AiReasoning = "quality-validator: located under test/sample path; LLM cross-validation unavailable — manual review required"
			suspicious++
		}
		if f.GetAiVerdict() == pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE {
			fp++
		}
	}
	if suspicious > 0 {
		log.Printf("[dsh-runtime][%s] Quality Validator: %d test/sample-path hits → NEEDS_MANUAL", req.GetTaskId(), suspicious)
	}
	s.completeAgentPhase(req.GetTaskId(), agent.AgentQualityValidator)

	// ---- 结果落盘: dsh-runtime→result BatchCreateFindings（09 §2 行；R4 幂等键） ----
	// ADR-134: 落盘失败不再静默（此前 AiFindingsCount=N 而 ids=[]，计数与落盘不一致无人知晓）
	ids := make([]string, 0, len(aiFindings))
	if len(aiFindings) > 0 && cfg.resultAddr != "" {
		storedIDs, serr := batchCreateFindingsToResult(cfg.resultAddr, req.GetTaskId(),
			req.GetMetadata().GetRequestId()+"-store", aiFindings)
		if serr != nil {
			return nil, status.Errorf(codes.Unavailable, "persist %d ai findings to result-service: %v", len(aiFindings), serr) // codes 通过 google.golang.org/grpc/codes 引入
		}
		ids = storedIDs
	}

	verifiedN := 0
	for _, f := range aiFindings {
		if f.GetAiVerdict() != pb.AIVerdict_AI_VERDICT_UNSPECIFIED {
			verifiedN++
		}
	}

	res := &pb.AIInferenceResult{
		AiFindingIds:       ids,
		AiFindingsCount:    int32(len(aiFindings)),
		VerifiedCount:      int32(verifiedN),
		FalsePositiveCount: int32(fp),
		Metrics: &pb.AnalysisMetrics{
			TotalDurationMs: time.Since(start).Milliseconds(),
			CodeAnalysisMs:  codeMs,
			LlmInferenceMs:  llmMs,
		},
	}
	return &pb.RunAIAnalysisResponse{Result: res, FixSuggestions: fixSuggestions}, nil
}

// buildFixSuggestion — Fix Advisor 出口（ADR-176 偏差③4c 债偿还 / ADR-183）：
// 沙箱 DSH 来源（fix_suggestion/diff_patch 经 mapSandboxFindings 填充，diff_patch 已
// 服务端校验重建，插件可 fuzz=0 锚定）→ LLM markdown + 补丁；RuleScan 降级/无建议 →
// "需人工处置"占位。诚实语义(2026-08-27 编造审计)：无真实 LLM 修复方案绝不编造——
// 占位 confidence 0.9 表述的是"该发现需处置"这一事实本身。
func buildFixSuggestion(f *pb.UnifiedFinding) *pb.FixSuggestion {
	if f.GetAiFixSuggestion() != "" || f.GetDiffPatch() != "" {
		return &pb.FixSuggestion{
			FindingId:   f.GetFindingId(),
			Description: f.GetAiFixSuggestion(),
			CodeFix:     f.GetDiffPatch(),
			DiffPatch:   f.GetDiffPatch(),
			Confidence:  f.GetAiConfidence(),
		}
	}
	return &pb.FixSuggestion{
		FindingId: f.GetFindingId(),
		Description: fmt.Sprintf("MANUAL_REVIEW_REQUIRED: %s at %s:%d (rule %s) — LLM fix-advisor not connected; no fabricated fix provided",
			f.GetTitle(), f.GetLocation().GetFilePath(), f.GetLocation().GetStartLine(), f.GetSourceRuleId()),
		Confidence: 0.9,
	}
}

// filepathWalkLite — 轻量项目遍历（Code Analyst 用）；回调返回 false 中止。
func filepathWalkLite(root string, fn func(path string, lines int32, lang string) bool) error {
	if root == "" {
		return nil
	}
	_, err := os.Stat(root)
	if err != nil {
		return err
	}
	return walkFn(root, fn)
}

// envOr 局部复制（service 包内已有一份全局的在此不可见时兜底）
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// fetchFindingsByIDs — 从 result-service 逐条取 finding 实体（09 §2: findings 权威存储）。
func fetchFindingsByIDs(ids []string) []*pb.UnifiedFinding {
	out := []*pb.UnifiedFinding{}
	if len(ids) == 0 {
		return out
	}
	conn, closeFn, err := dial(envOr("CODEAUDIT_RESULT_ADDR", "localhost:50058"))
	if err != nil {
		return out
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), cfgDurationSec("findings_fetch")) // 07 §8（ADR-137）
	defer cancel()
	for _, id := range ids {
		resp, err := client.GetFinding(ctx, &pb.GetFindingRequest{FindingId: id})
		if err != nil || resp.GetFinding() == nil {
			continue
		}
		out = append(out, resp.GetFinding())
	}
	return out
}

// fetchFindingsByTask — 按 task 翻页取全部 findings。
func fetchFindingsByTask(taskID string) ([]*pb.UnifiedFinding, error) {
	out := []*pb.UnifiedFinding{}
	conn, closeFn, err := dial(envOr("CODEAUDIT_RESULT_ADDR", "localhost:50058"))
	if err != nil {
		return out, err
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)
	cursor := ""
	for page := 0; page < 50; page++ { // 上限防失控；07 §5 分页
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.ListFindings(ctx, &pb.ListFindingsRequest{
			TaskId:     taskID,
			Pagination: &pb.PaginationRequest{PageSize: 100, Cursor: cursor}, // 07 §5 最大页
		})
		cancel()
		if err != nil {
			break
		}
		out = append(out, resp.GetFindings()...)
		if !resp.GetPagination().GetHasNext() || len(resp.GetFindings()) == 0 {
			break
		}
		cursor = resp.GetPagination().GetNextCursor()
	}
	return out, nil
}

// dedupKey — 同文件同起始行同 CWE 视为同一问题（融合去重口径的验证侧对齐，04 §3.2 阶段5）。
func dedupKey(f *pb.UnifiedFinding) string {
	return fmt.Sprintf("%s:%d:%s", f.GetLocation().GetFilePath(), f.GetLocation().GetStartLine(), f.GetCweId())
}

// qualityScore — 审核质量评分（0-100）：确认率主导（04 §3.4 整体评估语义）。
func qualityScore(total, confirmed int) int32 {
	if total == 0 {
		return 100 // 无发现：无可挑剔亦无信息，给满分审计口径
	}
	return int32(float64(confirmed) / float64(total) * 100)
}

// completeAgentPhase — 五角色阶段真实推进（ADR-134）：Agent 状态/迭代计数由真实流水线
// 驱动，GetAnalysisProgress 展示的是实际执行进度而非装饰性恒 0。
// 依据: 05 §4 五角色顺序（Code Analyst → Vuln Detector → Severity Assessor → Fix Advisor → Quality Validator）
func (s *DSHRuntimeServiceImpl) completeAgentPhase(taskID string, at agent.AgentType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.taskAgents[taskID] {
		if a.Type == at {
			_ = a.Iterate() // 真实阶段完成计数（每角色恰好执行一轮）
			a.SetStatus(agent.AgentStatusCompleted)
			return
		}
	}
}
