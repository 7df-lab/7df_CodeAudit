// Package handler implements gRPC handlers for sast-adapter-service.
// 依据: codeaudit_common.proto L1047-L1059 SASTFusionService
package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/internal/fusion"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// SASTFusionHandler implements SASTFusionService gRPC interface.
// 依据: codeaudit_common.proto L1047-L1059
type SASTFusionHandler struct {
	pb.UnimplementedSASTFusionServiceServer

	pipeline *fusion.FusionPipeline

	// 幂等存储: request_id -> response
	// 依据: 03 §2 幂等三态规则
	idempotencyStore sync.Map

	// Finding存储 (in-memory for now)
	findingStore sync.Map

	// externalResolver — 跨子域实体补全（ShareStore 注入；ADR-121）
	// 本地 store 未命中时从 SASTAdapterHandler 的扫描存储解析（同一部署单元）。
	externalResolver func(ids []string) []*pb.UnifiedFinding

	// resultAddr — result-service 地址：实体权威存储（09 §2）。
	// ADR-133: 本地与 adapter 扫描存储都未命中时，从 result-service GetFinding 兜底，
	// 保证 dsh-runtime 直写的 AI findings 也能被融合/对比解析到。
	resultAddr string
}

// resolveFindings — 本地 → adapter 扫描存储（ADR-121）→ result-service（09 §2 权威存储）。
// 返回 (解析到的findings, 缺失ID列表)。调用方对缺失 ID 必须显式报错而非静默缩小集合（ADR-133）。
func (h *SASTFusionHandler) resolveFindings(ids []string) ([]*pb.UnifiedFinding, []string) {
	out := make([]*pb.UnifiedFinding, 0, len(ids))
	missing := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if f, ok := h.findingStore.Load(id); ok {
			out = append(out, f.(*pb.UnifiedFinding))
			continue
		}
		if h.externalResolver != nil {
			if found := h.externalResolver([]string{id}); len(found) > 0 && found[0] != nil {
				h.findingStore.Store(id, found[0])
				out = append(out, found[0])
				continue
			}
		}
		if f := h.fetchFromResult(id); f != nil {
			h.findingStore.Store(id, f)
			out = append(out, f)
			continue
		}
		missing = append(missing, id)
	}
	return out, missing
}

// fetchFromResult — result-service GetFinding 兜底（ADR-133）。
func (h *SASTFusionHandler) fetchFromResult(id string) *pb.UnifiedFinding {
	if h.resultAddr == "" {
		return nil
	}
	conn, err := grpc.Dial(h.resultAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[fusion] dial result-service: %v", err)
		return nil
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetFinding(ctx, &pb.GetFindingRequest{FindingId: id})
	if err != nil {
		return nil
	}
	return resp.GetFinding()
}

// NewSASTFusionHandler creates a new fusion handler.
func NewSASTFusionHandler(resultAddr string) *SASTFusionHandler {
	return &SASTFusionHandler{
		pipeline:   fusion.NewFusionPipeline(),
		resultAddr: resultAddr,
	}
}

// FuseResults implements the main fusion RPC.
// 依据: codeaudit_common.proto L1048 (FuseResults)
// 依据: 03 §2 幂等规则（FuseResults 有 RequestMetadata）
func (h *SASTFusionHandler) FuseResults(ctx context.Context, req *pb.FuseResultsRequest) (*pb.FuseResultsResponse, error) {
	// 幂等检查 - 依据: 03 §2 三态规则
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata with request_id is required")
	}

	requestID := req.GetMetadata().GetRequestId()

	// 检查幂等缓存
	if cached, ok := h.idempotencyStore.Load(requestID); ok {
		// 同键同体→重放
		resp := cached.(*pb.FuseResultsResponse)
		return resp, nil
	}

	// 获取SAST/AI findings（本地→adapter存储→result-service 兜底）
	sastFindings, missingS := h.resolveFindings(req.GetSastFindingIds())
	aiFindings, missingA := h.resolveFindings(req.GetAiFindingIds())
	if len(missingS)+len(missingA) > 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"findings not found (result-service also unreachable/missing): %v%v",
			missingS, missingA)
	}

	// 执行融合流水线
	// 依据: 04 §3.2 阶段5 融合五阶段
	result, err := h.pipeline.Execute(ctx, req, sastFindings, aiFindings)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fusion failed: %v", err)
	}

	resp := &pb.FuseResultsResponse{
		Result: result,
	}

	// ADR-142: 融合结果真实回写 result-service（dedup_group/matched_findings/is_unique）——
	// 此前融合输出只随 RPC 返回、从不落盘，融合视图无内容、TP09-T3 交付名不副实。
	h.writeBackFusion(ctx, result, sastFindings, aiFindings)

	// 缓存响应用于幂等
	h.idempotencyStore.Store(requestID, resp)

	return resp, nil
}

// writeBackFusion — 按合并组回写融合字段（BatchUpdateFindings 白名单补丁，proto L1227）。
// 失败留日志不中断（融合本身已成功；回写失败下次重跑任务可补）。
func (h *SASTFusionHandler) writeBackFusion(ctx context.Context, result *pb.FusionResult, sastFindings, aiFindings []*pb.UnifiedFinding) {
	if h.resultAddr == "" || result.GetReport() == nil {
		return
	}
	conn, err := grpc.Dial(h.resultAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[fusion] write-back dial: %v", err)
		return
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	reqID := fmt.Sprintf("fusion-wb-%d", time.Now().UnixNano())

	patch := func(ids []string, kv map[string]string) {
		if len(ids) == 0 {
			return
		}
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		resp, err := client.BatchUpdateFindings(ctx2, &pb.BatchUpdateFindingsRequest{
			Metadata:   &pb.RequestMetadata{RequestId: reqID},
			FindingIds: ids,
			PatchJson:  kv,
		})
		if err != nil {
			log.Printf("[fusion] write-back %d ids: %v", len(ids), err)
			return
		}
		log.Printf("[fusion] write-back updated=%d failed=%v", resp.GetUpdatedCount(), resp.GetFailedIds())
	}

	inGroup := map[string]bool{}
	for _, g := range result.GetReport().GetMergeGroups() {
		members := g.GetMergedFindingIds()
		for _, id := range members {
			inGroup[id] = true
		}
		// 组内每个成员的 matched = 其他成员
		others := map[string]string{}
		for _, id := range members {
			var o []string
			for _, m := range members {
				if m != id {
					o = append(o, m)
				}
			}
			others[id] = strings.Join(o, ",")
		}
		for _, id := range members {
			patch([]string{id}, map[string]string{
				"dedup_group":      g.GetGroupId(),
				"is_unique":        "false",
				"matched_findings": others[id],
			})
		}
	}
	// 未合并发现 → is_unique=true
	var uniques []string
	for _, f := range append(append([]*pb.UnifiedFinding{}, sastFindings...), aiFindings...) {
		if !inGroup[f.GetFindingId()] {
			uniques = append(uniques, f.GetFindingId())
		}
	}
	if len(uniques) > 0 {
		patch(uniques, map[string]string{"is_unique": "true"})
	}
}

// AlignLocations aligns findings by location.
// 依据: codeaudit_common.proto L1049 (AlignLocations)
func (h *SASTFusionHandler) AlignLocations(ctx context.Context, req *pb.AlignLocationsRequest) (*pb.AlignLocationsResponse, error) {
	// ADR-133: 改走统一解析链（此前只读本地 store，经 adapter 扫描的 finding 永远不可见）
	findings, _ := h.resolveFindings(req.GetFindingIds())

	// 按位置分组
	// 依据: proto L38-L47 LocationInfo
	type locationKey struct {
		FilePath  string
		StartLine int32
	}

	groups := make(map[locationKey][]string)
	for _, f := range findings {
		loc := f.GetLocation()
		key := locationKey{
			FilePath:  loc.GetFilePath(),
			StartLine: loc.GetStartLine(),
		}
		groups[key] = append(groups[key], f.GetFindingId())
	}

	// 构建响应
	alignedGroups := make([]*pb.MergeGroup, 0)
	counter := 0
	for _, ids := range groups {
		if len(ids) > 1 {
			counter++
			// 依据: proto L463-L468 MergeGroup
			alignedGroups = append(alignedGroups, &pb.MergeGroup{
				GroupId:          fmt.Sprintf("align_%d", counter),
				MergedFindingIds: ids,
				PrimaryFindingId: ids[0],
				MergeReason:      "location_alignment",
			})
		}
	}

	return &pb.AlignLocationsResponse{
		AlignedGroups: alignedGroups,
	}, nil
}

// ClusterFindings clusters findings by similarity.
// 依据: codeaudit_common.proto L1050 (ClusterFindings)
// 诚实声明（ADR-133）: 当前实现是 (CWE, file_path) 精确键分组，similarity_threshold
// 参数未参与计算（无语义向量/相似度模型可用）；组标签注明真实分组依据。
func (h *SASTFusionHandler) ClusterFindings(ctx context.Context, req *pb.ClusterFindingsRequest) (*pb.ClusterFindingsResponse, error) {
	findings, _ := h.resolveFindings(req.GetFindingIds())

	// 简单聚类：按CWE+文件路径
	// 依据: proto L68 cwe_id 字段
	type clusterKey struct {
		CWEID    string
		FilePath string
	}

	clusters := make(map[clusterKey][]string)
	for _, f := range findings {
		loc := f.GetLocation()
		key := clusterKey{
			CWEID:    f.GetCweId(),
			FilePath: loc.GetFilePath(),
		}
		clusters[key] = append(clusters[key], f.GetFindingId())
	}

	// 构建响应
	mergeGroups := make([]*pb.MergeGroup, 0)
	counter := 0
	for _, ids := range clusters {
		if len(ids) > 1 {
			counter++
			mergeGroups = append(mergeGroups, &pb.MergeGroup{
				GroupId:          fmt.Sprintf("cluster_%d", counter),
				MergedFindingIds: ids,
				PrimaryFindingId: ids[0],
				MergeReason:      "cwe_file_exact_group (similarity_threshold not applied: exact-key clustering)",
			})
		}
	}

	return &pb.ClusterFindingsResponse{
		Clusters: mergeGroups,
	}, nil
}

// ResolveConflicts resolves conflicts between findings.
// 依据: codeaudit_common.proto L1051 (ResolveConflicts)
func (h *SASTFusionHandler) ResolveConflicts(ctx context.Context, req *pb.ResolveConflictsRequest) (*pb.ResolveConflictsResponse, error) {
	findings, _ := h.resolveFindings(req.GetFindingIds())

	resolved := make([]*pb.ConflictItem, 0)

	// 检测冲突
	for i := 0; i < len(findings); i++ {
		for j := i + 1; j < len(findings); j++ {
			f1, f2 := findings[i], findings[j]

			// 同位置不同严重级别
			if f1.GetLocation().GetFilePath() == f2.GetLocation().GetFilePath() &&
				f1.GetLocation().GetStartLine() == f2.GetLocation().GetStartLine() &&
				f1.GetSeverity() != f2.GetSeverity() {
				// 依据: proto L470-L474 ConflictItem
				resolved = append(resolved, &pb.ConflictItem{
					FindingIds:   []string{f1.GetFindingId(), f2.GetFindingId()},
					ConflictType: "severity_mismatch",
					Resolution:   "use_higher_severity",
				})
			}
		}
	}

	return &pb.ResolveConflictsResponse{
		Resolved: resolved,
	}, nil
}

// GetFusionConfig returns current fusion configuration.
// 依据: codeaudit_common.proto L1052 (GetFusionConfig) + L1438 FusionConfig
// ADR-137: 融合参数来自全局配置 fusion 段（07 §10 溯源），代码不留缺省。
func (h *SASTFusionHandler) GetFusionConfig(ctx context.Context, req *pb.GetFusionConfigRequest) (*pb.FusionConfig, error) {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global config: %v", err)
	}
	sim, err := cfg.Float("fusion.similarity_threshold")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global config: %v", err)
	}
	boost, err := cfg.Float("fusion.confidence_boost")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global config: %v", err)
	}
	flt, err := cfg.Float("fusion.filter_threshold")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global config: %v", err)
	}
	return &pb.FusionConfig{
		Config: map[string]string{
			"similarity_threshold": fmt.Sprintf("%v", sim),
			"confidence_boost":     fmt.Sprintf("%v", boost),
			"filter_threshold":     fmt.Sprintf("%v", flt),
		},
	}, nil
}

// UpdateFusionConfig updates fusion configuration.
// 依据: codeaudit_common.proto L1053 (UpdateFusionConfig)
func (h *SASTFusionHandler) UpdateFusionConfig(ctx context.Context, req *pb.UpdateFusionConfigRequest) (*pb.FusionConfig, error) {
	// 简单实现：返回传入的配置
	// 依据: proto L1438 FusionConfig
	return req.GetConfig(), nil
}

// precisionRecall — 以对方集合为参照的对称 precision/recall/F1（ADR-133 实现口径披露）:
//
//	SAST 侧: TP=both, FP=sast_only, FN=ai_only（把 AI 发现视为参照真值）
//	AI   侧: TP=both, FP=ai_only, FN=sast_only
//
// 双向互为参照的口径用于模式C"并行对比"（无独立 ground truth 时的自洽指标），
// 全量评估（DiverseVul 基准 F1）走 TP11/T2 evaluate_f1 链路，两者语义不同不混用。
func precisionRecall(tp, fp, fn int32) (precision, recall, f1 float32) {
	if tp+fp > 0 {
		precision = float32(tp) / float32(tp+fp)
	}
	if tp+fn > 0 {
		recall = float32(tp) / float32(tp+fn)
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return
}

// CompareResults compares SAST and AI results.
// 依据: codeaudit_common.proto L1056 (CompareResults)
func (h *SASTFusionHandler) CompareResults(ctx context.Context, req *pb.CompareResultsRequest) (*pb.CompareResultsResponse, error) {
	// 获取SAST/AI findings（本地→adapter存储→result-service 兜底, ADR-121/ADR-133）
	sastFindings, missingS := h.resolveFindings(req.GetSastFindingIds())
	aiFindings, missingA := h.resolveFindings(req.GetAiFindingIds())
	if len(missingS)+len(missingA) > 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"findings not found: %v%v", missingS, missingA)
	}

	// 四象限分类
	bothFound, sastOnly, aiOnly, disagreement := int32(0), int32(0), int32(0), int32(0)
	{
		type locationKey struct {
			FilePath  string
			StartLine int32
		}
		sastByLoc := make(map[locationKey]*pb.UnifiedFinding)
		for _, f := range sastFindings {
			loc := f.GetLocation()
			sastByLoc[locationKey{FilePath: loc.GetFilePath(), StartLine: loc.GetStartLine()}] = f
		}
		aiByLoc := make(map[locationKey]*pb.UnifiedFinding)
		for _, f := range aiFindings {
			loc := f.GetLocation()
			aiByLoc[locationKey{FilePath: loc.GetFilePath(), StartLine: loc.GetStartLine()}] = f
		}
		seenLocs := make(map[locationKey]bool)
		for key, sastF := range sastByLoc {
			seenLocs[key] = true
			if aiF, ok := aiByLoc[key]; ok {
				bothFound++
				if sastF.GetSeverity() != aiF.GetSeverity() {
					disagreement++
				}
			} else {
				sastOnly++
			}
		}
		for key := range aiByLoc {
			if !seenLocs[key] {
				aiOnly++
			}
		}
	}

	// 依据: proto L481-L489 ComparisonSummary + L491-L501 ComparisonMetrics（真实计算, ADR-133）
	sPrec, sRec, sF1 := precisionRecall(bothFound, sastOnly, aiOnly)
	aPrec, aRec, aF1 := precisionRecall(bothFound, aiOnly, sastOnly)
	summary := &pb.ComparisonSummary{
		SastTotal:    int32(len(sastFindings)),
		AiTotal:      int32(len(aiFindings)),
		BothFound:    bothFound,
		SastOnly:     sastOnly,
		AiOnly:       aiOnly,
		Disagreement: disagreement,
		Metrics: &pb.ComparisonMetrics{
			TotalUnique:   int32(len(sastFindings) + len(aiFindings) - int(bothFound)),
			SastPrecision: sPrec,
			SastRecall:    sRec,
			SastF1:        sF1,
			AiPrecision:   aPrec,
			AiRecall:      aRec,
			AiF1:          aF1,
		},
	}

	return &pb.CompareResultsResponse{
		Summary: summary,
	}, nil
}

// fetchTaskFindings — 从 result-service 拉取本任务全部发现（CalculateMetrics/报告用）。
func (h *SASTFusionHandler) fetchTaskFindings(taskID string) ([]*pb.UnifiedFinding, error) {
	if h.resultAddr == "" {
		return nil, status.Error(codes.FailedPrecondition, "result-service address not configured")
	}
	conn, err := grpc.Dial(h.resultAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "dial result-service: %v", err)
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	out := make([]*pb.UnifiedFinding, 0)
	cursor := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := client.ListFindings(ctx, &pb.ListFindingsRequest{
			TaskId:     taskID,
			Pagination: &pb.PaginationRequest{PageSize: 100, Cursor: cursor},
		})
		cancel()
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "ListFindings: %v", err)
		}
		out = append(out, resp.GetFindings()...)
		if !resp.GetPagination().GetHasNext() || resp.GetPagination().GetNextCursor() == "" {
			break
		}
		cursor = resp.GetPagination().GetNextCursor()
	}
	return out, nil
}

// CalculateMetrics calculates comparison metrics for a task's stored findings.
// 依据: codeaudit_common.proto L1057 (CalculateMetrics) + L491-L501 ComparisonMetrics
// ADR-133: 此前无论输入返回全 0（假数据冒充计算）；现按 source_tool 分类（ai_agent=AI，
// 其余=SAST，依据 proto L68 注释），真实计算四象限与双向 precision/recall/F1。
func (h *SASTFusionHandler) CalculateMetrics(ctx context.Context, req *pb.CalculateMetricsRequest) (*pb.ComparisonMetrics, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	findings, err := h.fetchTaskFindings(req.GetTaskId())
	if err != nil {
		return nil, err
	}

	sast, ai := splitBySource(findings)
	both, sastOnly, aiOnly := int32(0), int32(0), int32(0)
	{
		type locationKey struct {
			FilePath  string
			StartLine int32
		}
		keyOf := func(f *pb.UnifiedFinding) locationKey {
			return locationKey{f.GetLocation().GetFilePath(), f.GetLocation().GetStartLine()}
		}
		sastIdx, aiIdx := map[locationKey]bool{}, map[locationKey]bool{}
		for _, f := range sast {
			sastIdx[keyOf(f)] = true
		}
		for _, f := range ai {
			aiIdx[keyOf(f)] = true
		}
		for k := range sastIdx {
			if aiIdx[k] {
				both++
			} else {
				sastOnly++
			}
		}
		for k := range aiIdx {
			if !sastIdx[k] {
				aiOnly++
			}
		}
	}
	sPrec, sRec, sF1 := precisionRecall(both, sastOnly, aiOnly)
	aPrec, aRec, aF1 := precisionRecall(both, aiOnly, sastOnly)
	return &pb.ComparisonMetrics{
		TotalUnique:   int32(len(sast) + len(ai) - int(both)),
		SastPrecision: sPrec,
		SastRecall:    sRec,
		SastF1:        sF1,
		AiPrecision:   aPrec,
		AiRecall:      aRec,
		AiF1:          aF1,
	}, nil
}

// splitBySource — source_tool 分类（proto L68: codeql/semgrep/bandit/ai_agent）。
func splitBySource(findings []*pb.UnifiedFinding) (sast, ai []*pb.UnifiedFinding) {
	for _, f := range findings {
		if f.GetSourceTool() == "ai_agent" {
			ai = append(ai, f)
		} else {
			sast = append(sast, f)
		}
	}
	return
}

// GenerateComparisonReport generates a comparison report with real quadrants/metrics.
// 依据: codeaudit_common.proto L1058 (GenerateComparisonReport) + L1445 ComparisonReport
// ADR-133: 此前返回全 0 summary 假报告；现真实聚合。venn_data_url 留空——
// 不产出真实可取用的 URL 就不返回 URL（ADR-129 假URL教训）。
func (h *SASTFusionHandler) GenerateComparisonReport(ctx context.Context, req *pb.GenerateComparisonReportRequest) (*pb.ComparisonReport, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	findings, err := h.fetchTaskFindings(req.GetTaskId())
	if err != nil {
		return nil, err
	}
	sast, ai := splitBySource(findings)

	// 四象限（与 CompareResults 同口径）
	bothFound, sastOnly, aiOnly, disagreement := int32(0), int32(0), int32(0), int32(0)
	{
		type locationKey struct {
			FilePath  string
			StartLine int32
		}
		keyOf := func(f *pb.UnifiedFinding) locationKey {
			return locationKey{f.GetLocation().GetFilePath(), f.GetLocation().GetStartLine()}
		}
		sastIdx, aiIdx := map[locationKey]*pb.UnifiedFinding{}, map[locationKey]*pb.UnifiedFinding{}
		for _, f := range sast {
			sastIdx[keyOf(f)] = f
		}
		for _, f := range ai {
			aiIdx[keyOf(f)] = f
		}
		seen := map[locationKey]bool{}
		for k, sf := range sastIdx {
			seen[k] = true
			if af, ok := aiIdx[k]; ok {
				bothFound++
				if sf.GetSeverity() != af.GetSeverity() {
					disagreement++
				}
			} else {
				sastOnly++
			}
		}
		for k := range aiIdx {
			if !seen[k] {
				aiOnly++
			}
		}
	}
	sPrec, sRec, sF1 := precisionRecall(bothFound, sastOnly, aiOnly)
	aPrec, aRec, aF1 := precisionRecall(bothFound, aiOnly, sastOnly)

	return &pb.ComparisonReport{
		ReportId: fmt.Sprintf("cmp-%s", req.GetTaskId()),
		Summary: &pb.ComparisonSummary{
			SastTotal:    int32(len(sast)),
			AiTotal:      int32(len(ai)),
			BothFound:    bothFound,
			SastOnly:     sastOnly,
			AiOnly:       aiOnly,
			Disagreement: disagreement,
			Metrics: &pb.ComparisonMetrics{
				TotalUnique:   int32(len(sast) + len(ai) - int(bothFound)),
				SastPrecision: sPrec,
				SastRecall:    sRec,
				SastF1:        sF1,
				AiPrecision:   aPrec,
				AiRecall:      aRec,
				AiF1:          aF1,
			},
		},
		VennDataUrl: "", // 诚实留空: 无真实可取用资源不返回 URL（ADR-129）
	}, nil
}

// RegisterFinding stores a finding for later use.
func (h *SASTFusionHandler) RegisterFinding(f *pb.UnifiedFinding) {
	h.findingStore.Store(f.GetFindingId(), f)
}
