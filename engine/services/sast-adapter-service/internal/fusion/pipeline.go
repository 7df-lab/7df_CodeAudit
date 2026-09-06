// Package fusion implements the SAST fusion pipeline.
// 依据: codeaudit_common.proto L437-L461 (FusionResult/FusionMetrics/FusionReport)
// 依据: 04 §3.2 阶段5 融合五阶段（过滤误报→合并→去重对齐→置信度融合）
package fusion

import (
	"context"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FusionPipeline implements the 5-stage fusion pipeline.
// 依据: 04 §3.2 阶段5 融合五阶段
type FusionPipeline struct {
	stages []Stage
}

// Stage represents a single stage in the fusion pipeline.
type Stage interface {
	Name() string
	Execute(ctx context.Context, input *FusionContext) (*FusionContext, error)
}

// FusionContext holds intermediate state during fusion.
type FusionContext struct {
	// Input
	TaskID       string
	SASTFindings []*pb.UnifiedFinding
	AIFindings   []*pb.UnifiedFinding

	// Intermediate results
	FilteredSAST []*pb.UnifiedFinding
	FilteredAI   []*pb.UnifiedFinding
	Groups       []*pb.MergeGroup
	Conflicts    []*pb.ConflictItem
	Filtered     []*pb.FilteredItem

	// Output
	FusedFindings []*pb.UnifiedFinding
	Metrics       *FusionMetrics
}

// FusionMetrics tracks pipeline execution metrics.
// 依据: codeaudit_common.proto L446-L454
type FusionMetrics struct {
	DurationMs            int64
	InputSASTCount        int32
	InputAICount          int32
	OutputCount           int32
	MergedCount           int32
	RemovedFalsePositives int32
	AddedAIFindings       int32
}

// NewFusionPipeline creates a new fusion pipeline with all stages.
// 依据: 04 §3.2 阶段5 五阶段顺序
func NewFusionPipeline() *FusionPipeline {
	return &FusionPipeline{
		stages: []Stage{
			NewFalsePositiveFilterStage(), // 阶段1: 过滤误报
			NewMergeStage(),               // 阶段2: 合并
			NewDedupAlignStage(),          // 阶段3: 去重对齐
			NewConflictResolveStage(),     // 阶段4: 冲突解决
			NewConfidenceFusionStage(),    // 阶段5: 置信度融合
		},
	}
}

// Execute runs the complete fusion pipeline.
// 依据: 04 §3.2 阶段5 + 07 §10 降级策略（融合失败→输出未融合原始结果）
func (p *FusionPipeline) Execute(ctx context.Context, req *pb.FuseResultsRequest, sastFindings, aiFindings []*pb.UnifiedFinding) (*pb.FusionResult, error) {
	startTime := time.Now()

	// 依据: 07 §10 降级策略
	// 融合失败时输出未融合原始结果
	// Initialize context
	fusionCtx := &FusionContext{
		TaskID:       req.GetTaskId(),
		SASTFindings: sastFindings,
		AIFindings:   aiFindings,
	}

	// Execute each stage sequentially
	// ADR-133 修复：panic 此前被吞掉后返回 (nil,nil)，调用方判 err==nil 后返回空结果冒充成功；
	// 现 panic 转为 error → 走 07 §10 未融合降级路径。
	for _, stage := range p.stages {
		select {
		case <-ctx.Done():
			return nil, status.Error(codes.DeadlineExceeded, "fusion pipeline timeout")
		default:
		}

		var err error
		fusionCtx, err = runStage(ctx, stage, fusionCtx)
		if err != nil {
			// 依据: 07 §10 融合失败→输出未融合原始结果
			return p.buildFallbackResult(fusionCtx, startTime, err), nil
		}
	}

	// Build final result
	return p.buildResult(fusionCtx, startTime), nil
}

// runStage — 单阶段执行，panic 转 error（ADR-133）。
func runStage(ctx context.Context, stage Stage, fctx *FusionContext) (out *FusionContext, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "fusion stage %s panic: %v", stage.Name(), r)
		}
	}()
	return stage.Execute(ctx, fctx)
}

// buildResult constructs the FusionResult from pipeline output.
// 依据: codeaudit_common.proto L437-L444
func (p *FusionPipeline) buildResult(ctx *FusionContext, startTime time.Time) *pb.FusionResult {
	duration := time.Since(startTime).Milliseconds()

	// Collect fused finding IDs
	fusedIDs := make([]string, 0, len(ctx.FusedFindings))
	for _, f := range ctx.FusedFindings {
		fusedIDs = append(fusedIDs, f.GetFindingId())
	}

	// 计算 AI 独有发现：AI findings 不在任何合并组中的
	// 依据: proto L453 added_ai_findings = AI独有发现（未与SAST匹配的）
	mergedAIFindings := make(map[string]bool)
	for _, group := range ctx.Groups {
		for _, fid := range group.GetMergedFindingIds() {
			mergedAIFindings[fid] = true
		}
	}
	addedAI := int32(0)
	for _, f := range ctx.AIFindings {
		if !mergedAIFindings[f.GetFindingId()] {
			addedAI++
		}
	}

	// 依据: codeaudit_common.proto L437-L444 FusionResult
	return &pb.FusionResult{
		FusedFindingIds:       fusedIDs,
		TotalCount:            int32(len(fusedIDs)),
		RemovedFalsePositives: ctx.Metrics.RemovedFalsePositives,
		AddedAiFindings:       addedAI,
		Metrics: &pb.FusionMetrics{
			DurationMs:            duration,                          // 依据: proto L447
			InputSastCount:        ctx.Metrics.InputSASTCount,        // 依据: proto L448
			InputAiCount:          ctx.Metrics.InputAICount,          // 依据: proto L449
			OutputCount:           int32(len(fusedIDs)),              // 依据: proto L450
			MergedCount:           ctx.Metrics.MergedCount,           // 依据: proto L451
			RemovedFalsePositives: ctx.Metrics.RemovedFalsePositives, // 依据: proto L452
			AddedAiFindings:       addedAI,                           // 依据: proto L453
		},
		Report: &pb.FusionReport{
			MergeGroups: ctx.Groups,    // 依据: proto L457
			Conflicts:   ctx.Conflicts, // 依据: proto L458
			Filtered:    ctx.Filtered,  // 依据: proto L459
		},
	}
}

// buildFallbackResult returns unfused results on pipeline failure.
// 依据: 07 §10 降级策略矩阵（融合失败→输出未融合原始结果）
func (p *FusionPipeline) buildFallbackResult(ctx *FusionContext, startTime time.Time, err error) *pb.FusionResult {
	// Combine all findings without fusion
	allFindings := make([]*pb.UnifiedFinding, 0)
	allFindings = append(allFindings, ctx.SASTFindings...)
	allFindings = append(allFindings, ctx.AIFindings...)

	ids := make([]string, 0, len(allFindings))
	for _, f := range allFindings {
		ids = append(ids, f.GetFindingId())
	}

	return &pb.FusionResult{
		FusedFindingIds: ids,
		TotalCount:      int32(len(ids)),
		Metrics: &pb.FusionMetrics{
			DurationMs:     time.Since(startTime).Milliseconds(),
			InputSastCount: int32(len(ctx.SASTFindings)),
			InputAiCount:   int32(len(ctx.AIFindings)),
			OutputCount:    int32(len(allFindings)),
		},
		Report: &pb.FusionReport{},
	}
}
