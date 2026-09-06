package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb" // 依据: proto L924 DeleteFinding returns google.protobuf.Empty
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 依据: codeaudit_common.proto L920-L935 ResultService
type ResultServiceImpl struct {
	storageAddr string // ADR-200 补遗: storage 归档地址（空=跳过）
	pb.UnimplementedResultServiceServer
	repo repository.FindingRepository
	// producer — finding.verdict.updated 事件出口（ADR-006/09 §2）。
	// ADR-135 修复: 此前 producer 已构造但从未接线，verdict 事件链实际断裂。
	producer VerdictEventPublisher
}

// VerdictEventPublisher — 事件发布最小接口（解耦 Kafka 实现与单测）。
type VerdictEventPublisher interface {
	PublishVerdictUpdated(ctx context.Context, finding *model.Finding, oldVerdict string, updatedBy string) error
}

func NewResultServiceImpl(repo repository.FindingRepository) *ResultServiceImpl {
	return &ResultServiceImpl{repo: repo}
}

// SetEventProducer — main.go 接线（ADR-135）。
func (s *ResultServiceImpl) SetEventProducer(p VerdictEventPublisher) { s.producer = p }

// CreateFinding - 依据: codeaudit_common.proto L921 + L1216
func (s *ResultServiceImpl) CreateFinding(ctx context.Context, req *pb.CreateFindingRequest) (*pb.AuditFinding, error) {
	// ADR-135: metadata 缺失时幂等键落空（GetByRequestIDAndFindingID 失效）→ 显式拒绝
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}
	if req.GetFinding() == nil || req.GetFinding().GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding with id is required")
	}

	// 幂等检查 - 依据: 03 §2 三态规则 / proto L1216 metadata 字段
	existing, err := s.repo.GetByRequestIDAndFindingID(req.GetMetadata().GetRequestId(), req.GetFinding().GetFindingId())
	if err == nil && existing != nil {
		// 同键同体 -> 重放
		return s.modelToAudit(existing), nil
	}

	// 依据: proto L1216 CreateFindingRequest 有 Metadata，直接创建
	finding := &model.Finding{
		ID:         req.GetFinding().GetFindingId(),
		TaskID:     req.GetFinding().GetTaskId(),
		ToolName:   req.GetFinding().GetSourceTool(),
		RuleID:     req.GetFinding().GetSourceRuleId(),
		CWE:        req.GetFinding().GetCweId(),
		Severity:   req.GetFinding().GetSeverity().String(),
		Message:    req.GetFinding().GetDescription(),
		FilePath:   req.GetFinding().GetLocation().GetFilePath(),
		LineNumber: int(req.GetFinding().GetLocation().GetStartLine()),
		// ADR-141→ADR-201: 代码上下文=proto L67 source_raw（原始输出 JSON 序列化）。
		// ADR-141 曾复用 code_snippet 死列承载，ADR-201 列/字段正名 source_raw 同名直存
		SourceRaw: string(req.GetFinding().GetSourceRaw()),
		DedupGroup:  req.GetFinding().GetDedupGroup(),
		// ADR-142: 融合字段随 CreateFinding 同步（与 Batch 分支同口径）
		MatchedFindings: strings.Join(req.GetFinding().GetMatchedFindings(), ","),
		IsUnique:        req.GetFinding().GetIsUnique(),
		Verdict:         req.GetFinding().GetAiVerdict().String(),
		// ADR-183: 修复建议两通道落盘（此前 ai_fix_suggestion 在此层被丢弃）
		AiFixSuggestion: req.GetFinding().GetAiFixSuggestion(),
		DiffPatch:       req.GetFinding().GetDiffPatch(),
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		RequestID:       req.GetMetadata().GetRequestId(),
	}

	if err := s.repo.Create(finding); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create finding: %v", err)
	}

	return s.modelToAudit(finding), nil
}

// GetFinding - 依据: codeaudit_common.proto L922
func (s *ResultServiceImpl) GetFinding(ctx context.Context, req *pb.GetFindingRequest) (*pb.AuditFinding, error) {
	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}

	finding, err := s.repo.GetByID(req.GetFindingId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "finding not found: %v", err)
	}

	return s.modelToAudit(finding), nil
}

// UpdateFinding - 依据: codeaudit_common.proto L923
func (s *ResultServiceImpl) UpdateFinding(ctx context.Context, req *pb.UpdateFindingRequest) (*pb.AuditFinding, error) {
	if req.GetFinding() == nil || req.GetFinding().GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding with id is required")
	}

	// proto L1218 UpdateFindingRequest 无 metadata 字段(E2已上报的设计缺口),
	// 幂等以 finding_id 定位 + 更新语义天然幂等(同体重复更新结果一致)

	existing, err := s.repo.GetByID(req.GetFinding().GetFindingId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "finding not found: %v", err)
	}

	// Update fields
	existing.ToolName = req.GetFinding().GetSourceTool()
	existing.RuleID = req.GetFinding().GetSourceRuleId()
	existing.CWE = req.GetFinding().GetCweId()
	existing.Severity = req.GetFinding().GetSeverity().String()
	existing.Message = req.GetFinding().GetDescription()
	existing.FilePath = req.GetFinding().GetLocation().GetFilePath()
	existing.LineNumber = int(req.GetFinding().GetLocation().GetStartLine())
	existing.Verdict = req.GetFinding().GetAiVerdict().String()
	existing.UpdatedAt = time.Now()

	if err := s.repo.Update(existing); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update finding: %v", err)
	}

	return s.modelToAudit(existing), nil
}

// DeleteFinding - 依据: codeaudit_common.proto L924
func (s *ResultServiceImpl) DeleteFinding(ctx context.Context, req *pb.DeleteFindingRequest) (*emptypb.Empty, error) {
	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}

	if err := s.repo.Delete(req.GetFindingId()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete finding: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// ListFindings - 依据: codeaudit_common.proto L925 + L1220-L1221
func (s *ResultServiceImpl) ListFindings(ctx context.Context, req *pb.ListFindingsRequest) (*pb.ListFindingsResponse, error) {
	pageSize := int(req.GetPagination().GetPageSize()) // proto L1220 pagination 字段
	if pageSize <= 0 {
		pageSize = cfgPageSizeDefault() // ADR-137: proto L227 默认20（值在全局配置 result.page_size_default）
	}
	if pageSize > 100 {
		pageSize = cfgPageSizeMax() // ADR-137: proto L227 最大100（值在全局配置 result.page_size_max）
	}

	cursor := &model.Cursor{LastID: "", Limit: pageSize}
	if req.GetPagination().GetCursor() != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.GetPagination().GetCursor())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid cursor: %v", err)
		}
		if err := json.Unmarshal(decoded, cursor); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid cursor format: %v", err)
		}
	}

	findings, nextCursor, err := s.repo.List(cursor.LastID, pageSize, req.GetTaskId(), "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list findings: %v", err)
	}

	resp := &pb.ListFindingsResponse{
		Findings: make([]*pb.UnifiedFinding, len(findings)), // proto L1221 repeated UnifiedFinding
		Pagination: &pb.PaginationResponse{ // 依据: proto L230-L234
			HasNext: nextCursor != "",
		},
	}
	for i, f := range findings {
		resp.Findings[i] = s.modelToUnified(f)
	}
	resp.Pagination.NextCursor = cursorFromLastID(nextCursor, pageSize)

	return resp, nil
}

// BatchCreateFindings - 依据: codeaudit_common.proto L926 + L1222-L1226
func (s *ResultServiceImpl) BatchCreateFindings(ctx context.Context, req *pb.BatchCreateFindingsRequest) (*pb.BatchCreateFindingsResponse, error) {
	// 幂等键必填 - proto L1223 metadata 字段
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}

	resp := &pb.BatchCreateFindingsResponse{
		FindingIds: make([]string, 0, len(req.GetFindings())),
	}

	for _, finding := range req.GetFindings() {
		// 幂等检查 - 依据: 03 §2 三态规则
		existing, err := s.repo.GetByRequestIDAndFindingID(req.GetMetadata().GetRequestId(), finding.GetFindingId())
		if err == nil && existing != nil {
			// 同键同体 -> 重放(不重复创建)
			resp.FindingIds = append(resp.FindingIds, existing.ID)
			continue
		}

		f := &model.Finding{
			ID:         finding.GetFindingId(),
			TaskID:     finding.GetTaskId(),
			ToolName:   finding.GetSourceTool(),
			RuleID:     finding.GetSourceRuleId(),
			CWE:        finding.GetCweId(),
			Severity:   finding.GetSeverity().String(),
			Message:    finding.GetDescription(),
			FilePath:   finding.GetLocation().GetFilePath(),
			LineNumber: int(finding.GetLocation().GetStartLine()),
			// ADR-141→ADR-201: 代码上下文随 finding 持久化（source_raw 同名直存）
			SourceRaw:       string(finding.GetSourceRaw()),
			Reasoning:       finding.GetAiReasoning(),
			DedupGroup:      finding.GetDedupGroup(),
			MatchedFindings: strings.Join(finding.GetMatchedFindings(), ","),
			IsUnique:        finding.GetIsUnique(),
			Verdict:         finding.GetAiVerdict().String(),
			// ADR-183: 修复建议两通道落盘（与 Create 分支同口径）
			AiFixSuggestion: finding.GetAiFixSuggestion(),
			DiffPatch:       finding.GetDiffPatch(),
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			RequestID:       req.GetMetadata().GetRequestId(),
		}

		if err := s.repo.Create(f); err != nil {
			// ADR-198: 失败不静默（ADR-133 精神）——此前 PG 列宽不足时逐条静默丢失
			log.Printf("[result] BatchCreateFindings: %s create failed: %v", f.ID, err)
			resp.FailedCount++ // proto L1226 failed_count
			continue
		}
		resp.FindingIds = append(resp.FindingIds, f.ID)
	}

	return resp, nil
}

// BatchUpdateFindings - 依据: codeaudit_common.proto L927 + L1227-L1232
// ADR-142 实现（此前 Unimplemented 兜底——融合结果回写无落盘通道，融合视图无内容）。
// 语义: patch_json（字段名→新值）应用到 finding_ids 中的每一条；白名单字段防误写。
func (s *ResultServiceImpl) BatchUpdateFindings(ctx context.Context, req *pb.BatchUpdateFindingsRequest) (*pb.BatchUpdateFindingsResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}
	if len(req.GetFindingIds()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "finding_ids is required")
	}
	if len(req.GetPatchJson()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "patch_json is required")
	}

	resp := &pb.BatchUpdateFindingsResponse{}
	for _, id := range req.GetFindingIds() {
		f, err := s.repo.GetByID(id)
		if err != nil {
			log.Printf("[result] BatchUpdateFindings: %s not found/err: %v", id, err)
			resp.FailedIds = append(resp.FailedIds, id)
			continue
		}
		for k, v := range req.GetPatchJson() {
			switch k { // 白名单=proto UnifiedFinding 可变融合/结论字段
			case "dedup_group":
				f.DedupGroup = v
			case "matched_findings":
				f.MatchedFindings = v // CSV 串
			case "is_unique":
				f.IsUnique = v == "true"
			case "verdict":
				f.Verdict = v
			// confidence 无独立存储字段——白名单暂不接受（诚实：不接受即不写）
			default:
				// 未知字段忽略（诚实：不静默写库）
			}
		}
		f.UpdatedAt = time.Now()
		f.RequestID = req.GetMetadata().GetRequestId()
		if err := s.repo.Update(f); err != nil {
			log.Printf("[result] BatchUpdateFindings: update %s failed: %v", id, err)
			resp.FailedIds = append(resp.FailedIds, id)
			continue
		}
		resp.UpdatedCount++
	}
	return resp, nil
}

// BatchUpdateVerdict - 依据: codeaudit_common.proto L928 + L1232-L1237
func (s *ResultServiceImpl) BatchUpdateVerdict(ctx context.Context, req *pb.BatchUpdateVerdictRequest) (*pb.BatchUpdateVerdictResponse, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}

	resp := &pb.BatchUpdateVerdictResponse{}

	for _, findingID := range req.GetFindingIds() { // proto L1234 repeated string finding_ids
		finding, err := s.repo.GetByID(findingID)
		if err != nil {
			log.Printf("[result] BatchUpdateVerdict: finding %s not found/err: %v", findingID, err)
			continue // 失败不计入 updated_count(L1238)
		}
		oldVerdict := finding.Verdict

		finding.Verdict = req.GetVerdict().String()
		finding.UpdatedAt = time.Now()
		finding.RequestID = req.GetMetadata().GetRequestId()

		if err := s.repo.Update(finding); err != nil {
			log.Printf("[result] BatchUpdateVerdict: update %s failed: %v", findingID, err)
			continue
		}
		resp.UpdatedCount++
		// ADR-006 事件链真实接线（ADR-135）：verdict 变更才发布
		if s.producer != nil && oldVerdict != finding.Verdict {
			if perr := s.producer.PublishVerdictUpdated(ctx, finding, oldVerdict, "batch"); perr != nil {
				log.Printf("[result] PublishVerdictUpdated %s: %v (DB 已更新, 事件投递失败)", findingID, perr)
			}
		}
	}

	return resp, nil
}

// UpdateVerdict - 依据: codeaudit_common.proto L929 + L1239
func (s *ResultServiceImpl) UpdateVerdict(ctx context.Context, req *pb.UpdateVerdictRequest) (*pb.AuditFinding, error) {
	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}

	finding, err := s.repo.GetByID(req.GetFindingId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "finding not found: %v", err)
	}
	oldVerdict := finding.Verdict

	finding.Verdict = req.GetVerdict().String()
	finding.UpdatedAt = time.Now()
	if req.GetReasoning() != "" {
		finding.Reasoning = req.GetReasoning() // 依据: proto L1240 reasoning 字段（ADR-135 落库）
	}

	if err := s.repo.Update(finding); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update verdict: %v", err)
	}

	// ADR-006 事件链真实接线（ADR-135）
	if s.producer != nil && oldVerdict != finding.Verdict {
		if perr := s.producer.PublishVerdictUpdated(ctx, finding, oldVerdict, "rpc"); perr != nil {
			log.Printf("[result] PublishVerdictUpdated %s: %v (DB 已更新, 事件投递失败)", finding.ID, perr)
		}
	}

	return s.modelToAudit(finding), nil
}

// GetFindingsByVerdict - 依据: codeaudit_common.proto L930 + L1240
func (s *ResultServiceImpl) GetFindingsByVerdict(ctx context.Context, req *pb.GetFindingsByVerdictRequest) (*pb.ListFindingsResponse, error) {
	pageSize := int(req.GetPagination().GetPageSize()) // proto L1240 pagination 字段
	if pageSize <= 0 {
		pageSize = cfgPageSizeDefault() // ADR-137: proto L227 默认20（值在全局配置 result.page_size_default）
	}
	if pageSize > 100 {
		pageSize = cfgPageSizeMax() // ADR-137: proto L227 最大100（值在全局配置 result.page_size_max）
	}

	cursor := &model.Cursor{LastID: "", Limit: pageSize}
	if req.GetPagination().GetCursor() != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.GetPagination().GetCursor())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid cursor: %v", err)
		}
		if err := json.Unmarshal(decoded, cursor); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid cursor format: %v", err)
		}
	}

	findings, nextCursor, err := s.repo.ListByVerdict(req.GetVerdict().String(), cursor.LastID, pageSize)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list findings: %v", err)
	}

	resp := &pb.ListFindingsResponse{
		Findings: make([]*pb.UnifiedFinding, len(findings)),
		Pagination: &pb.PaginationResponse{
			HasNext: nextCursor != "",
		},
	}
	for i, f := range findings {
		resp.Findings[i] = s.modelToUnified(f)
	}
	resp.Pagination.NextCursor = cursorFromLastID(nextCursor, pageSize)

	return resp, nil
}

// GetTaskResultStats - 依据: codeaudit_common.proto L931 + L1242 ResultStats
func (s *ResultServiceImpl) GetTaskResultStats(ctx context.Context, req *pb.GetTaskResultStatsRequest) (*pb.ResultStats, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	stats, err := s.repo.GetStatsByTaskID(req.GetTaskId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get stats: %v", err)
	}

	return &pb.ResultStats{
		TaskId:     req.GetTaskId(),
		Total:      int32(stats.TotalFindings), // proto L1243 total
		BySeverity: stats.BySeverity,           // proto L1244
		ByCwe:      stats.ByCwe,
		ByVerdict:  stats.ByVerdict,
	}, nil
}

// ExportFindings - 依据: codeaudit_common.proto L932 + L1245-L1246
// SetStorageAddr — ADR-200 补遗: findings 导出归档地址（env CODEAUDIT_STORAGE_ADDR）。
func (s *ResultServiceImpl) SetStorageAddr(addr string) { s.storageAddr = addr }

func (s *ResultServiceImpl) ExportFindings(ctx context.Context, req *pb.ExportFindingsRequest) (*pb.ExportFindingsResponse, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	// 依据: 03 §5 分页规范 - 获取所有 findings
	pageSize := 100 // 07 §5 最大 page_size
	cursor := ""
	var allFindings []*model.Finding

	for {
		findings, nextCursor, err := s.repo.List(cursor, pageSize, req.GetTaskId(), "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list findings: %v", err)
		}

		allFindings = append(allFindings, findings...)

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	// Export as JSON - 依据: 03 §5 导出格式; proto L1246 export_url 单字段
	// 2026-08-27 修复(编造审计): 旧实现 marshal 后丢弃数据(_ = data)并返回不存在的
	// export:// 假URL——冒充导出。现真实落盘到数据目录, URL 指向真实文件。
	data, err := json.MarshalIndent(allFindings, "", "  ")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal findings: %v", err)
	}

	exportDir := os.Getenv("CODEAUDIT_EXPORT_DIR")
	if exportDir == "" {
		// ADR-137: 值在全局配置 result.export_dir（沙箱 /tmp 为进程私有视图, 跨进程不可审计）
		cfg, cerr := codeauditcfg.Default()
		if cerr != nil {
			return nil, status.Errorf(codes.Internal, "load global config: %v", cerr)
		}
		if exportDir, cerr = cfg.Str("result.export_dir"); cerr != nil {
			return nil, status.Errorf(codes.Internal, "load global config: %v", cerr)
		}
	}
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create export dir: %v", err)
	}
	exportPath := filepath.Join(exportDir, fmt.Sprintf("findings-%s.json", req.GetTaskId()))
	if err := os.WriteFile(exportPath, data, 0o644); err != nil {
		return nil, status.Errorf(codes.Internal, "write export file: %v", err)
	}
	log.Printf("[result-service] exported %d findings for task %s → %s",
		len(allFindings), req.GetTaskId(), exportPath)

	// ADR-200 补遗: 导出件归档 storage（exports/ 前缀 → 默认桶），export_url 指向
	// 真实对象地址；归档失败如实降级为本地 file:// 引用（文件已真实落盘）。
	exportURL := "file://" + exportPath
	if s.storageAddr != "" {
		objPath := fmt.Sprintf("exports/findings-%s.json", req.GetTaskId())
		if u, uerr := uploadToStorage(ctx, s.storageAddr, objPath, "application/json", data); uerr != nil {
			log.Printf("[result-service] export archive FAILED for %s: %v (local copy intact)", req.GetTaskId(), uerr)
		} else {
			exportURL = u
			log.Printf("[result-service] export archived: %s", u)
		}
	}

	return &pb.ExportFindingsResponse{
		ExportUrl: exportURL,
	}, nil
}

// SubmitFindingFeedback - 依据: codeaudit_common.proto L935 + L1247-L1253
func (s *ResultServiceImpl) SubmitFindingFeedback(ctx context.Context, req *pb.SubmitFindingFeedbackRequest) (*pb.SubmitFindingFeedbackResponse, error) {
	// 幂等键必填 - proto L1248 metadata 字段
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}

	if req.GetFindingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "finding_id is required")
	}

	// 幂等检查 - 依据: 03 §2 三态规则
	existing, err := s.repo.GetFeedbackByRequestID(req.GetMetadata().GetRequestId())
	if err == nil && existing != nil {
		// 同键同体 -> 重放
		return &pb.SubmitFindingFeedbackResponse{
			FeedbackId: existing.ID,
			Accepted:   true, // proto L1254 accepted
		}, nil
	}

	// Create feedback
	feedback := &model.FindingFeedback{
		ID:        fmt.Sprintf("fb_%s_%s", req.GetFindingId(), req.GetMetadata().GetRequestId()),
		FindingID: req.GetFindingId(),
		Comment:   req.GetComment(),
		CreatedAt: time.Now(),
		RequestID: req.GetMetadata().GetRequestId(),
	}

	if err := s.repo.CreateFeedback(feedback); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create feedback: %v", err)
	}

	return &pb.SubmitFindingFeedbackResponse{
		FeedbackId: feedback.ID,
		Accepted:   true,
	}, nil
}

// Helper: modelToAudit converts model.Finding to proto AuditFinding (proto L1215 AuditFinding 包装 UnifiedFinding)
func (s *ResultServiceImpl) modelToAudit(f *model.Finding) *pb.AuditFinding {
	return &pb.AuditFinding{
		Finding: s.modelToUnified(f),
	}
}

// Helper: modelToUnified converts model.Finding to proto UnifiedFinding (proto L54-L90 真实字段)
func (s *ResultServiceImpl) modelToUnified(f *model.Finding) *pb.UnifiedFinding {
	return &pb.UnifiedFinding{
		FindingId:    f.ID,
		TaskId:       f.TaskID,
		SourceTool:   f.ToolName,
		SourceRuleId: f.RuleID,
		CweId:        f.CWE,
		Severity:     pb.Severity(pb.Severity_value[f.Severity]),
		Description:  f.Message,
		Location: &pb.LocationInfo{
			FilePath:  f.FilePath,
			StartLine: int32(f.LineNumber),
		},
		AiVerdict:   pb.AIVerdict(pb.AIVerdict_value[f.Verdict]),
		AiReasoning: f.Reasoning,
		// ADR-183: 修复建议两通道回读（插件 diff_patch 优先 / ai_fix_suggestion 降级消费）
		AiFixSuggestion: f.AiFixSuggestion,
		DiffPatch:       f.DiffPatch,
		// ADR-141→ADR-201: source_raw 回读（列与字段同名直存）
		SourceRaw: []byte(f.SourceRaw),
		// ADR-142: 融合字段回读（融合视图数据源）
		DedupGroup:      f.DedupGroup,
		MatchedFindings: splitCSV(f.MatchedFindings),
		IsUnique:        f.IsUnique,
		// ADR-152: 复核时间回读（页面"复核状态"列数据源；此前恒为空）
		UpdatedAt: timestamppb.New(f.UpdatedAt),
	}
}

// splitCSV — 逗号分隔串转列表（空串→空列表）。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// Helper: cursorFromLastID 把仓储返回的裸 lastID 包装为 03 §5 契约游标
// （base64(JSON{last_id,limit})），与 ListFindings/GetFindingsByVerdict 的解码侧对称。
// 修复记录: 2026-08-27 端到端审核中发现编码/解码不对称导致第二页必然
// "invalid cursor format"（首例触发场景: 单任务发现数 >100 的分页拉取）。
func cursorFromLastID(lastID string, limit int) string {
	if lastID == "" {
		return ""
	}
	encoded, err := json.Marshal(model.Cursor{LastID: lastID, Limit: limit})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// ---- ADR-137: 分页缺省值来自全局配置（proto L227 口径），代码不留业务缺省 ----

func cfgResultInt(key string) int {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("result-service config: %v (ADR-137)", err))
	}
	v, err := cfg.Int(key)
	if err != nil {
		panic(fmt.Sprintf("result-service config: %v (ADR-137)", err))
	}
	return v
}

func cfgPageSizeDefault() int { return cfgResultInt("result.page_size_default") }
func cfgPageSizeMax() int     { return cfgResultInt("result.page_size_max") }
