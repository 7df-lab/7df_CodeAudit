// Package service implements the DSHRuntimeService gRPC server.
// 依据: codeaudit_common.proto L956-L973 DSHRuntimeService 定义
// 依据: 05 §4 五角色推理流程（Code Analyst → Vuln Detector → Severity Assessor → Fix Advisor → Quality Validator）
// 依据: 07 §8 超时矩阵 — Task→DSHRuntime 30m, DSH会话存活 30m
// 依据: 07 §8.1 Agent 迭代上限
// 依据: 06 §3 沙箱配置
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/agent"
	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
	"github.com/codeaudit/services/dsh-runtime-service/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// DSHRuntimeServiceImpl implements DSHRuntimeService.
type DSHRuntimeServiceImpl struct {
	pb.UnimplementedDSHRuntimeServiceServer
	mu       sync.RWMutex
	sessions *session.Manager
	// 任务 → Agent 实例映射
	taskAgents map[string][]*agent.Agent
	// 幂等存储：request_id → {fingerprint, response}（03 §2 三态）
	idempotency map[string]*idemEntry
	// AI 交互日志留存（ADR-168）：沙箱 bridge SSE 原始帧，任务级
	aiLogs *aiLogStore
}

// idemEntry — 幂等条目：请求体指纹 + 首次响应。
type idemEntry struct {
	fingerprint string
	resp        interface{}
}

// fingerprintAIAnalysis — RunAIAnalysis 请求体指纹（03 §2 同键异体判定）。
func fingerprintAIAnalysis(req *pb.RunAIAnalysisRequest) string {
	return fmt.Sprintf("%s|%s|%d|%v", req.GetTaskId(), req.GetProjectPath(), req.GetScanMode(), req.GetSastFindingIds())
}

// NewDSHRuntimeService creates a new DSHRuntimeServiceImpl.
func NewDSHRuntimeService() *DSHRuntimeServiceImpl {
	return &DSHRuntimeServiceImpl{
		sessions:    session.NewManager(),
		taskAgents:  make(map[string][]*agent.Agent),
		idempotency: make(map[string]*idemEntry),
		aiLogs:      sharedAILogs, // 包级单例：与 analyzeViaSandbox 写入侧同源（ADR-168）
	}
}

// RunAIAnalysis initiates the five-agent analysis pipeline.
// 依据: codeaudit_common.proto L958
// 依据: 05 §4 五角色推理流程
// 依据: 07 §8 — Task→DSHRuntime 30m timeout
// 依据: 07 §8.1 — Agent 迭代上限
func (s *DSHRuntimeServiceImpl) RunAIAnalysis(ctx context.Context, req *pb.RunAIAnalysisRequest) (*pb.RunAIAnalysisResponse, error) {
	// R4: 检查幂等键
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}

	// 幂等检查: 依据: 03 §2 三态规则（同键同体重放 / 同键异体 ALREADY_EXISTS）
	reqID := req.GetMetadata().GetRequestId()
	fp := fingerprintAIAnalysis(req)
	s.mu.Lock()
	if existing, ok := s.idempotency[reqID]; ok {
		s.mu.Unlock()
		if existing.fingerprint == fp {
			resp, ok := existing.resp.(*pb.RunAIAnalysisResponse)
			if ok {
				log.Printf("Idempotent replay for RunAIAnalysis %s", reqID)
				return resp, nil
			}
		}
		return nil, status.Error(codes.AlreadyExists, "request_id used with a different request body (03 §2)")
	}
	s.mu.Unlock()

	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	// 创建会话
	// 依据: 07 §8 — DSH 会话存活 30m
	sess := s.sessions.CreateSession(reqID, taskID)

	// 创建五 Agent
	// 依据: 05 §4 五角色推理流程
	agentTypes := []agent.AgentType{
		agent.AgentCodeAnalyst,
		agent.AgentVulnDetector,
		agent.AgentSeverityAssessor,
		agent.AgentFixAdvisor,
		agent.AgentQualityValidator,
	}

	var agents []*agent.Agent
	for _, at := range agentTypes {
		a, err := agent.NewAgent(at)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create agent %s: %v", at, err)
		}
		agents = append(agents, a)
	}

	s.mu.Lock()
	s.taskAgents[taskID] = agents
	s.mu.Unlock()

	// 执行五 Agent 流水线
	// 依据: 05 §4 — 顺序执行：Code Analyst → Vuln Detector → Severity Assessor → Fix Advisor → Quality Validator
	// 接线: 真实调用 ai-inference Chat/RuleScan（03 §1.6），产出经 result-service 落盘（09 §2）
	result, err := s.runFiveAgentPipeline(ctx, req, sess.ID)
	if err != nil {
		return nil, err
	}

	// 存储幂等结果
	s.mu.Lock()
	s.idempotency[reqID] = &idemEntry{fingerprint: fp, resp: result}
	s.mu.Unlock()

	return result, nil
}

// VerifySASTResults verifies SAST findings using AI.
// 依据: codeaudit_common.proto L959 — 模式B 阶段4a（读代码→数据流→净化器→可达性）
// 接线: 从 result-service 按 finding_ids 取实体，逐条启发式验证后回传 VerifiedFinding[]
func (s *DSHRuntimeServiceImpl) VerifySASTResults(ctx context.Context, req *pb.VerifySASTResultsRequest) (*pb.VerifySASTResultsResponse, error) {
	// R4: 检查幂等键
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}

	// 从 result-service 拉取待验证实体（09 §2：findings 权威存储在 result-service）
	findings := fetchFindingsByIDs(req.GetFindingIds())

	// ADR-173 沙箱优先：整项目代码上传沙箱磁盘，逐条 prompt 交 DSH 自行阅读完整源码
	// 判定真伪（数据流/净化/可达性在项目全文上核对），逐条取回结论后销毁沙箱。
	if req.GetProjectPath() != "" {
		emit := taskLogSink(req.GetTaskId(), "sandbox")
		if verified, serr := verifyViaSandbox(ctx, req.GetTaskId(), req.GetProjectPath(), findings, emit); serr == nil {
			return &pb.VerifySASTResultsResponse{Verified: verified}, nil
		} else if !errors.Is(serr, sandbox.ErrDisabled) {
			// ADR-175: ai-inference 直连链删除（自 ADR-140 起只会拿到 fallback 占位）——
			// 沙箱不可用即如实全批 NEEDS_MANUAL，绝不冒充 AI 判定（人类硬性要求显式降级）。
			log.Printf("[dsh-runtime][%s] sandbox verify unavailable (%v) → all NEEDS_MANUAL (degraded, 07 §10)",
				req.GetTaskId(), serr)
			emit("warn", fmt.Sprintf("沙箱 DSH 不可用（%v）→ 逐条验证降级：全部 NEEDS_MANUAL 待人工（非 AI 语义判定）", serr))
		}
	}
	verified := make([]*pb.VerifiedFinding, 0, len(findings))
	for _, f := range findings {
		verified = append(verified, &pb.VerifiedFinding{
			OriginalFindingId: f.GetFindingId(),
			Verdict:           pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL,
			Confidence:        0.3,
			Reasoning:         "[降级] 沙箱 DSH 不可用，未做 AI 语义审查，需人工复核（07 §10）",
		})
	}
	return &pb.VerifySASTResultsResponse{Verified: verified}, nil
}

// SearchMissedVulns searches for vulnerabilities missed by SAST.
// 依据: codeaudit_common.proto L960 — 模式B 阶段4b（未覆盖区域/业务逻辑）
// ADR-165 接线: AI 原生挖掘优先（aiNativeDiscovery, LLM 直接阅读源码找洞）；
// LLM 不可用/回退/无语料 → RuleScan 规则兜底（诚实降级），剔除已报告项 → ai_agent 新发现
func (s *DSHRuntimeServiceImpl) SearchMissedVulns(ctx context.Context, req *pb.SearchMissedVulnsRequest) (*pb.SearchMissedVulnsResponse, error) {
	// R4: 检查幂等键
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}

	missed := []*pb.UnifiedFinding{}

	known, _ := fetchFindingsByTask(req.GetTaskId())
	knownKeys := map[string]bool{}
	for _, k := range known {
		knownKeys[dedupKey(k)] = true
	}

	// ADR-165: 契约新增 project_path 字段——修复此前靠无人设置的环境变量断线
	projectPath := req.GetProjectPath()
	if projectPath == "" {
		projectPath = os.Getenv("CODEAUDIT_PROJECT_REPO_PATH") // 兼容旧口径
	}
	if projectPath == "" {
		log.Printf("[dsh-runtime][%s] SearchMissedVulns: no project_path (request/config/env); cannot scan", req.GetTaskId())
	}

	// ADR-173 沙箱优先：整项目上传沙箱磁盘，单 prompt 交 DSH 全项目审计并取回发现。
	// 失败/未启用 → ai-native 直连 → RuleScan 的既有降级链。
	if projectPath != "" {
		emit := taskLogSink(req.GetTaskId(), "sandbox")
		if sbxFindings, serr := discoverViaSandbox(ctx, req.GetTaskId(), projectPath, knownKeys, emit); serr == nil {
			if len(sbxFindings) > 0 {
				if _, serr := batchCreateFindingsToResult(cfgAddr("result", "CODEAUDIT_RESULT_ADDR"), // ADR-137
					req.GetTaskId(), req.GetMetadata().GetRequestId()+"-store", sbxFindings); serr != nil {
					log.Printf("[dsh-runtime][%s] persist %d sandbox findings FAILED: %v",
						req.GetTaskId(), len(sbxFindings), serr)
				}
			}
			log.Printf("[dsh-runtime][%s] sandbox discovery: %d new findings (project=%s)",
				req.GetTaskId(), len(sbxFindings), projectPath)
			return &pb.SearchMissedVulnsResponse{MissedFindings: sbxFindings}, nil
		} else if !errors.Is(serr, sandbox.ErrDisabled) {
			log.Printf("[dsh-runtime][%s] sandbox discovery unavailable (%v) → ai-native/RuleScan chain (07 §10)",
				req.GetTaskId(), serr)
		}
	}

	// ADR-175: ai-inference 删除——ai-native 直连段（自 ADR-140 起拿到的只有 fallback 占位）
	// 与 RuleScan RPC 段整段移除；沙箱不可用 → 内置 RuleScan 本地兜底（三重降级标注）。
	if projectPath != "" {
		rsFindings := rulescanFallback(req.GetTaskId(), projectPath,
			"SearchMissedVulns sandbox path unavailable", taskLogSink(req.GetTaskId(), "dsh-runtime"))
		for _, f := range rsFindings {
			if !knownKeys[dedupKey(f)] {
				missed = append(missed, f)
			}
		}
		if len(missed) > 0 {
			if _, serr := batchCreateFindingsToResult(cfgAddr("result", "CODEAUDIT_RESULT_ADDR"), // ADR-137
				req.GetTaskId(), req.GetMetadata().GetRequestId()+"-store", missed); serr != nil {
				log.Printf("[dsh-runtime][%s] persist %d rulescan-fallback findings FAILED: %v",
					req.GetTaskId(), len(missed), serr)
			}
		}
		return &pb.SearchMissedVulnsResponse{MissedFindings: missed}, nil
	}
	return &pb.SearchMissedVulnsResponse{MissedFindings: missed}, nil // 04 §6 降级不阻断
}

// ReviewSASTResults performs comprehensive SAST review (模式D).
// 依据: codeaudit_common.proto L961 + 04 §3.4（整体评估→逐条审核→汇总）
func (s *DSHRuntimeServiceImpl) ReviewSASTResults(ctx context.Context, req *pb.ReviewSASTResultsRequest) (*pb.ReviewSASTResultsResponse, error) {
	// R4: 检查幂等键
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}

	findings := fetchFindingsByIDs(req.GetSastFindingIds())

	// ADR-140/166: 沙箱可用时优先逐条 LLM 交叉验证（04 §3.4 步骤2 真实语义审核）
	if reviews2, opinions2, err := sandboxReview(ctx, req.GetTaskId(), findings, taskLogSink(req.GetTaskId(), "sandbox")); err == nil {
		total := int32(len(findings))
		confirm := 0
		for _, r := range reviews2 {
			if r.GetOpinion() == pb.ReviewOpinion_REVIEW_OPINION_CONFIRM {
				confirm++
			}
		}
		conclusion := pb.ReviewConclusion_REVIEW_CONCLUSION_PASS_WITH_NOTES
		if total > 0 {
			ratio := float64(confirm) / float64(total)
			switch {
			case ratio < 0.5:
				conclusion = pb.ReviewConclusion_REVIEW_CONCLUSION_FAIL
			case ratio < 0.9:
				conclusion = pb.ReviewConclusion_REVIEW_CONCLUSION_NEEDS_REVIEW
			}
		}
		log.Printf("[dsh-runtime][%s] review via sandbox DSH: %d reviews", req.GetTaskId(), len(reviews2))
		return &pb.ReviewSASTResultsResponse{TaskId: req.GetTaskId(), Report: &pb.AuditReviewReport{
			TaskId:        req.GetTaskId(),
			TotalFindings: total,
			Overall: &pb.OverallAssessment{
				QualityScore: qualityScore(int(total), confirm),
				Conclusion:   conclusion,
				Summary: fmt.Sprintf("%d findings: %d confirmed via sandbox DSH → %s",
					total, confirm, pb.ReviewConclusion(int32(conclusion)).String()),
			},
			Reviews: reviews2,
			Stats:   &pb.ReviewStats{ByOpinion: opinions2},
			Metadata: &pb.ReviewMetadata{
				ModelUsed: "dsh-headless (OpenShell sandbox, gateway inference route)",
			},
		}}, nil
	} else if !errors.Is(err, sandbox.ErrDisabled) {
		log.Printf("[dsh-runtime][%s] sandbox review unavailable (%s), falling back to manual chain",
			req.GetTaskId(), sandboxErrHint(err))
	}

	// ADR-175: ai-inference 直连链删除（自 ADR-140 起只会拿到 fallback 占位，从无真实
	// 语义审查）；沙箱通道不可用时整批如实 NEEDS_MANUAL（人类硬性要求显式降级标注）。
	taskLogSink(req.GetTaskId(), "dsh-runtime")("warn",
		"沙箱 DSH 审核不可用 → 逐条审核降级：全部 SUGGEST_REVIEW（非 AI 语义判定，07 §10）")
	log.Printf("[dsh-runtime][%s] sandbox review unavailable → all NEEDS_MANUAL (degraded)", req.GetTaskId())
	reviews := []*pb.FindingReview{}
	opinionCount := map[string]int32{}
	confirm, llmDone, llmUnavailable := 0, 0, len(findings)
	for _, f := range findings {
		opinion := pb.ReviewOpinion_REVIEW_OPINION_SUGGEST_REVIEW
		opinionCount[opinion.String()]++
		reviews = append(reviews, &pb.FindingReview{
			FindingId:  f.GetFindingId(),
			Opinion:    opinion,
			Confidence: 0.3,
			Reasoning:  "[降级] 沙箱 DSH 不可用，未做 AI 语义审查，需人工复核（07 §10）",
		})
	}

	total := int32(len(findings))
	// 结论仅由 LLM 确认率驱动; 全程无 LLM 参与时不发"通过"结论
	conclusion := pb.ReviewConclusion_REVIEW_CONCLUSION_PASS_WITH_NOTES
	if total > 0 {
		ratio := float64(confirm) / float64(total)
		switch {
		case llmUnavailable == int(total):
			conclusion = pb.ReviewConclusion_REVIEW_CONCLUSION_NEEDS_REVIEW // 零语义审查, 不予通过结论
		case ratio < 0.5:
			conclusion = pb.ReviewConclusion_REVIEW_CONCLUSION_FAIL
		case ratio < 0.9:
			conclusion = pb.ReviewConclusion_REVIEW_CONCLUSION_NEEDS_REVIEW
		}
	}
	summary := fmt.Sprintf("%d findings: %d confirmed by LLM, %d llm-reviewed, %d llm-unavailable → %s",
		total, confirm, llmDone, llmUnavailable, pb.ReviewConclusion(int32(conclusion)).String())
	log.Printf("[dsh-runtime][%s] review summary: %s", req.GetTaskId(), summary)

	report := &pb.AuditReviewReport{
		TaskId:        req.GetTaskId(),
		TotalFindings: total,
		Overall: &pb.OverallAssessment{
			// 质量分=LLM 已确认占比; 零 LLM 参与时为 0（不虚报）
			QualityScore: qualityScore(int(total), confirm),
			Conclusion:   conclusion,
			Summary:      summary,
		},
		Reviews: reviews,
		Stats:   &pb.ReviewStats{ByOpinion: opinionCount},
		Metadata: &pb.ReviewMetadata{
			// ADR-134: 如实反映实际推理路径（此前写死 rulescan-heuristic，
			// 即使部分发现经真实 LLM 审查也标注失真）
			ModelUsed: modelUsedLabel(llmDone, llmUnavailable, int(total)),
		},
	}
	return &pb.ReviewSASTResultsResponse{TaskId: req.GetTaskId(), Report: report}, nil
}

// modelUsedLabel — 审核元数据：如实描述实际推理路径构成（ADR-134）。
func modelUsedLabel(llmDone, llmUnavailable, total int) string {
	switch {
	case llmDone == total && total > 0:
		return "dsh-headless (OpenShell sandbox, gateway inference route)"
	case llmDone > 0:
		return fmt.Sprintf("mixed(sandbox=%d, fallback=%d)", llmDone, llmUnavailable)
	default:
		return "degraded: sandbox DSH unavailable, no AI review performed (ADR-175)"
	}
}

// GetAnalysisProgress returns the progress of an analysis.
// 依据: codeaudit_common.proto L969
func (s *DSHRuntimeServiceImpl) GetAnalysisProgress(ctx context.Context, req *pb.GetAnalysisProgressRequest) (*pb.AnalysisProgress, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	s.mu.RLock()
	agents, ok := s.taskAgents[taskID]
	s.mu.RUnlock()

	if !ok {
		return &pb.AnalysisProgress{
			TaskId: taskID,
			Status: pb.AnalysisStatus_ANALYSIS_STATUS_UNSPECIFIED,
		}, nil
	}

	// 计算总体进度
	var totalIter, maxIter int32
	currentAgent := ""
	for _, a := range agents {
		totalIter += a.GetIteration()
		maxIter += a.Config.MaxIterations
		if a.GetStatus() == agent.AgentStatusRunning {
			currentAgent = string(a.Type)
		}
	}

	percent := float32(0)
	if maxIter > 0 {
		percent = float32(totalIter) / float32(maxIter) * 100
	}

	return &pb.AnalysisProgress{
		TaskId:       taskID,
		Status:       pb.AnalysisStatus_ANALYSIS_STATUS_RUNNING,
		Percent:      percent,
		CurrentAgent: currentAgent,
		Iteration:    totalIter,
	}, nil
}

// WatchAnalysisProgress streams analysis progress updates.
// 依据: codeaudit_common.proto L970
func (s *DSHRuntimeServiceImpl) WatchAnalysisProgress(req *pb.WatchAnalysisProgressRequest, stream pb.DSHRuntimeService_WatchAnalysisProgressServer) error {
	return status.Error(codes.Unimplemented, "WatchAnalysisProgress streaming not yet implemented")
}

// GetSessionStatus returns the status of a session.
// 依据: codeaudit_common.proto L971
func (s *DSHRuntimeServiceImpl) GetSessionStatus(ctx context.Context, req *pb.GetSessionStatusRequest) (*pb.SessionStatus, error) {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	sess, err := s.sessions.GetSession(sessionID)
	if err != nil {
		return &pb.SessionStatus{
			SessionId: sessionID,
			State:     "not_found",
		}, nil
	}

	state, activeSandboxes := sess.GetStatus()
	return &pb.SessionStatus{
		SessionId:       sessionID,
		State:           state,
		ActiveSandboxes: activeSandboxes,
	}, nil
}

// CancelAnalysis cancels an ongoing analysis.
// 依据: codeaudit_common.proto L972
func (s *DSHRuntimeServiceImpl) CancelAnalysis(ctx context.Context, req *pb.CancelAnalysisRequest) (*emptypb.Empty, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	// 取消所有关联的 agents
	s.mu.Lock()
	if agents, ok := s.taskAgents[taskID]; ok {
		for _, a := range agents {
			a.SetStatus(agent.AgentStatusFailed)
		}
	}
	s.mu.Unlock()

	return &emptypb.Empty{}, nil
}

// PauseAnalysis — ADR-200: 挂起任务会话（回合边界生效；无活动会话也接受预约，
// 后续启动的会话立即挂起——Pause 与会话启动任意时序收敛）。
func (s *DSHRuntimeServiceImpl) PauseAnalysis(ctx context.Context, req *pb.PauseAnalysisRequest) (*emptypb.Empty, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	sandbox.PauseSession(taskID)
	log.Printf("[dsh-runtime] session paused (gate engaged): task=%s", taskID)
	return &emptypb.Empty{}, nil
}

// ResumeAnalysis — ADR-200: 释放会话闸门，继续后续回合。
func (s *DSHRuntimeServiceImpl) ResumeAnalysis(ctx context.Context, req *pb.ResumeAnalysisRequest) (*emptypb.Empty, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	sandbox.ResumeSession(taskID)
	log.Printf("[dsh-runtime] session resumed: task=%s", taskID)
	return &emptypb.Empty{}, nil
}

// GetAIInteractionLog — AI 交互日志增量读（ADR-168）：游标为字节偏移，与
// GetTaskLogs 的 log_id 游标同构。读 RPC（无幂等键要求，03 §2）。
// 依据: codeaudit_common.proto DSHRuntimeService/GetAIInteractionLog
func (s *DSHRuntimeServiceImpl) GetAIInteractionLog(ctx context.Context, req *pb.GetAIInteractionLogRequest) (*pb.GetAIInteractionLogResponse, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	s.aiLogs.mu.Lock()
	entry := s.aiLogs.logs[taskID]
	s.aiLogs.mu.Unlock()
	if entry == nil {
		// 本进程从未写过该任务：查磁盘兜底（进程重启后任务日志仍在）；
		// 文件也不存在 → complete=false（AI 阶段可能尚未开始，日志仍会增长——
		// 不得把"未知"谎报为"已收束"，否则 GUI 会过早停止轮询）。
		chunk, next, total := s.aiLogs.readDiskOnly(taskID, req.GetCursor(), req.GetMaxBytes())
		return &pb.GetAIInteractionLogResponse{Chunk: chunk, NextCursor: next, Complete: total > 0, TotalBytes: total}, nil
	}
	maxBytes := req.GetMaxBytes()
	if maxBytes == 0 {
		maxBytes = aiLogReadDefault
	}
	chunk, next, complete, total := entry.read(req.GetCursor(), maxBytes)
	return &pb.GetAIInteractionLogResponse{
		Chunk:      chunk,
		NextCursor: next,
		Complete:   complete,
		TotalBytes: total,
	}, nil
}

// StreamAIInteractionLog — 交互日志订阅流（ADR-189）：写入即推增量（gui WebSocket
// 上游真推送），字节游标语义与 GetAIInteractionLog 完全一致。首帧=游标之后的现有
// 内容；此后 write/finish 信号即时唤醒；complete=true 帧发出后关流。本进程无该任务
// 条目时等待条目出现（AI 阶段可能尚未开始，GUI 常在任务启动即订阅）；磁盘有遗留
// 文件（进程重启场景）则一帧收束。读 RPC（无幂等键要求，03 §2）。
func (s *DSHRuntimeServiceImpl) StreamAIInteractionLog(req *pb.StreamAIInteractionLogRequest, stream pb.DSHRuntimeService_StreamAIInteractionLogServer) error {
	taskID := req.GetTaskId()
	if taskID == "" {
		return status.Error(codes.InvalidArgument, "task_id is required")
	}
	cursor := req.GetCursor()
	maxBytes := req.GetMaxBytes()
	if maxBytes == 0 {
		maxBytes = aiLogReadDefault
	}

	s.aiLogs.mu.Lock()
	entry := s.aiLogs.logs[taskID]
	s.aiLogs.mu.Unlock()
	if entry == nil {
		// 磁盘遗留文件：一帧全量收束（complete=true）
		if chunk, next, total := s.aiLogs.readDiskOnly(taskID, cursor, maxBytes); total > 0 {
			return stream.Send(&pb.GetAIInteractionLogResponse{Chunk: chunk, NextCursor: next, Complete: true, TotalBytes: total})
		}
		// 等条目出现（500ms 查注册表；ctx 取消即退出——订阅端断开不泄漏）
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for entry == nil {
			select {
			case <-stream.Context().Done():
				return nil
			case <-tick.C:
				s.aiLogs.mu.Lock()
				entry = s.aiLogs.logs[taskID]
				s.aiLogs.mu.Unlock()
			}
		}
	}

	notify, unsub := entry.subscribe()
	defer unsub()
	for {
		chunk, next, complete, total := entry.read(cursor, maxBytes)
		if len(chunk) > 0 || complete {
			if err := stream.Send(&pb.GetAIInteractionLogResponse{
				Chunk: chunk, NextCursor: next, Complete: complete, TotalBytes: total,
			}); err != nil {
				return err
			}
			cursor = next
			if complete {
				return nil // 终帧已发，流收束
			}
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-notify:
		}
	}
}
