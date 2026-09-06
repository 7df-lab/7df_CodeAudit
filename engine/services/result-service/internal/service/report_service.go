package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb" // 依据: proto L1263 Report.generated_at
)

// 依据: codeaudit_common.proto L942-L948 ReportService
// 依据: ADR-006 Kafka 主路径 / gRPC 降级路径
type ReportServiceImpl struct {
	pb.UnimplementedReportServiceServer
	repo        repository.ReportRepository
	findings    repository.FindingRepository // 报告内容真实聚合本任务 findings（2026-08-27 修复占位符）
	storageAddr string                       // ADR-199: storage-service gRPC 地址（空=跳过归档）
}

func NewReportServiceImpl(repo repository.ReportRepository) *ReportServiceImpl {
	return &ReportServiceImpl{repo: repo}
}

// SetFindingRepository 注入 findings 数据源（同部署单元内的仓储实例，01 §4.2）。
// 修复记录: 2026-08-27 端到端审核发现 generateReportContent 为硬编码占位符，
// 报告恒报 total_findings=0——与真实落盘数据脱节。
// SetStorageAddr — ADR-199: storage-service 地址（env CODEAUDIT_STORAGE_ADDR；空=跳过归档）。
func (s *ReportServiceImpl) SetStorageAddr(addr string) { s.storageAddr = addr }

// uploadChunkSize — UploadFile 客户端流分块大小（64KiB，常规 gRPC 消息安全档）。
const uploadChunkSize = 64 << 10

func (s *ReportServiceImpl) SetFindingRepository(fr repository.FindingRepository) {
	s.findings = fr
}

// GenerateReport - 依据: codeaudit_common.proto L943 + L1255-L1260
// 幂等: R4 + 03 §2（降级路径需要幂等）
func (s *ReportServiceImpl) GenerateReport(ctx context.Context, req *pb.GenerateReportRequest) (*pb.GenerateReportResponse, error) {
	// 依据: 03 §2 幂等 - GenerateReportRequest.metadata (L1256)
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.request_id is required")
	}

	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}

	// 幂等检查 - 依据: 03 §2 三态规则
	// ADR-135 修复: 1) 同键异体必须 ALREADY_EXISTS 而非盲重放（补 body 校验）；
	// 2) 此前 FAILED 报告占住 request_id 导致重试永远拿到失败报告（粘死）——
	//    失败报告不参与重放，允许同键重试重新生成。
	existing, err := s.repo.GetReportByRequestID(req.GetMetadata().GetRequestId())
	if err == nil && existing != nil && existing.Status != "FAILED" {
		// 同键同体 -> 重放；同键异体 -> ALREADY_EXISTS
		if existing.TaskID != req.GetTaskId() {
			return nil, status.Errorf(codes.AlreadyExists,
				"request_id %s already used for a different task (03 §2)", req.GetMetadata().GetRequestId())
		}
		return &pb.GenerateReportResponse{
			Result: &pb.ReportResult{ // 依据: proto L507-L513 ReportResult
				ReportId: existing.ID,
			},
		}, nil
	}

	// 依据: ADR-006 异步主路径（Kafka） vs 降级路径（gRPC 直调）
	// 此处为降级路径实现：直接生成报告
	// ADR-142: 格式枚举驱动内容渲染（HTML=人工可读在线报告；JSON=机器消费）
	templateName := req.GetTemplateId()
	if templateName == "" && req.GetFormat() == pb.ReportFormat_REPORT_FORMAT_HTML {
		templateName = "html"
	}
	report := &model.Report{
		ID:       fmt.Sprintf("report_%s_%s", req.GetTaskId(), req.GetMetadata().GetRequestId()),
		TaskID:   req.GetTaskId(),
		Template: templateName,
		// ADR-142: 格式持久化（此前漏设→列表恒"未知"）
		Format:    req.GetFormat().String(),
		Status:    "GENERATING",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RequestID: req.GetMetadata().GetRequestId(),
	}

	// Generate report content based on template
	content, err := s.generateReportContent(req.GetTaskId(), templateName)
	if err != nil {
		report.Status = "FAILED"
		report.ErrorMessage = err.Error()
		if err := s.repo.CreateReport(report); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to save report: %v", err)
		}
		return &pb.GenerateReportResponse{
			Result: &pb.ReportResult{
				ReportId: report.ID,
			},
		}, nil
	}

	report.Content = content
	report.Status = "COMPLETED"
	completedAt := time.Now()
	report.CompletedAt = &completedAt

	if err := s.repo.CreateReport(report); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save report: %v", err)
	}

	// ADR-199: 09 §2 行 result → storage UploadFile(报告) 的真实落地——报告归档进
	// MinIO（FilePath=reports/<report_id>.json → reports 桶），url 从占位 report://
	// 换成真实对象地址。降级诚实: storage 未配置/不可达时仅 WARN，报告本体仍在 PG。
	if s.storageAddr != "" {
		if url, uerr := s.uploadReportToStorage(ctx, report); uerr != nil {
			log.Printf("[report] storage archive FAILED for %s: %v (PG copy intact)", report.ID, uerr)
		} else {
			report.Url = url
			if uerr2 := s.repo.UpdateReport(report); uerr2 != nil {
				log.Printf("[report] storage url writeback FAILED for %s: %v", report.ID, uerr2)
			} else {
				log.Printf("[report] archived to storage: %s", url)
			}
		}
	}

	return &pb.GenerateReportResponse{
		Result: &pb.ReportResult{
			ReportId: report.ID,
		},
	}, nil
}

// uploadReportToStorage — 报告内容经 gRPC 客户端流上传 storage（UploadFile 契约：
// 首块带 file_path/content_type，后续块纯数据）。返回可直接引用的对象 URI。
func (s *ReportServiceImpl) uploadReportToStorage(ctx context.Context, r *model.Report) (string, error) {
	conn, err := grpc.NewClient(s.storageAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("dial storage-service: %w", err)
	}
	defer conn.Close()
	sc, cerr := pb.NewStorageServiceClient(conn).UploadFile(ctx)
	if cerr != nil {
		return "", cerr
	}
	objPath := "reports/" + r.ID + ".json"
	first := true
	for begin, i := 0, 0; begin < len(r.Content) || i == 0; i++ {
		end := begin + uploadChunkSize
		if end > len(r.Content) {
			end = len(r.Content)
		}
		chunk := &pb.UploadFileChunk{Data: []byte(r.Content[begin:end])}
		if first {
			chunk.FirstChunk = true
			chunk.FilePath = objPath
			chunk.ContentType = "application/json"
			first = false
		}
		if serr := sc.Send(chunk); serr != nil {
			return "", fmt.Errorf("send chunk %d: %w", i, serr)
		}
		if begin >= len(r.Content) {
			break
		}
		begin = end
	}
	stored, err := sc.CloseAndRecv()
	if err != nil {
		return "", fmt.Errorf("CloseAndRecv: %w", err)
	}
	return "minio://reports/" + stored.GetFilePath(), nil
}

// GetReport - 依据: codeaudit_common.proto L944 + L1263
func (s *ReportServiceImpl) GetReport(ctx context.Context, req *pb.GetReportRequest) (*pb.Report, error) {
	if req.GetReportId() == "" {
		return nil, status.Error(codes.InvalidArgument, "report_id is required")
	}

	report, err := s.repo.GetReportByID(req.GetReportId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "report not found: %v", err)
	}

	return s.modelToProto(report), nil
}

// ListReports - 依据: codeaudit_common.proto L945 + L1264-L1265
func (s *ReportServiceImpl) ListReports(ctx context.Context, req *pb.ListReportsRequest) (*pb.ListReportsResponse, error) {
	// 依据: 03 §5 分页规范 / proto L225-L228 PaginationRequest
	pageSize := int(req.GetPagination().GetPageSize())
	if pageSize <= 0 {
		pageSize = 20 // 07 §5 默认 page_size
	}
	if pageSize > 100 {
		pageSize = 100 // 07 §5 最大 page_size
	}

	cursor := req.GetPagination().GetCursor()

	reports, nextCursor, err := s.repo.ListReports(cursor, pageSize, req.GetTaskId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list reports: %v", err)
	}

	resp := &pb.ListReportsResponse{
		Reports: make([]*pb.Report, len(reports)),
		Pagination: &pb.PaginationResponse{ // 依据: proto L230-L234
			NextCursor: nextCursor,
			HasNext:    nextCursor != "",
		},
	}
	for i, r := range reports {
		resp.Reports[i] = s.modelToProto(r)
	}

	return resp, nil
}

// ListTemplates - 依据: codeaudit_common.proto L946 + L1266-L1268
func (s *ReportServiceImpl) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	// 依据: 03 §5 分页规范 / proto L225-L228
	pageSize := int(req.GetPagination().GetPageSize())
	if pageSize <= 0 {
		pageSize = 20 // 07 §5 默认 page_size
	}
	if pageSize > 100 {
		pageSize = 100 // 07 §5 最大 page_size
	}

	templates, err := s.repo.ListTemplates(pageSize)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list templates: %v", err)
	}

	resp := &pb.ListTemplatesResponse{
		Templates: make([]*pb.ReportTemplate, len(templates)),
	}
	for i, t := range templates {
		resp.Templates[i] = s.templateModelToProto(t)
	}

	return resp, nil
}

// GetTemplate - 依据: codeaudit_common.proto L947
func (s *ReportServiceImpl) GetTemplate(ctx context.Context, req *pb.GetTemplateRequest) (*pb.ReportTemplate, error) {
	if req.GetTemplateId() == "" {
		return nil, status.Error(codes.InvalidArgument, "template_id is required")
	}

	template, err := s.repo.GetTemplateByID(req.GetTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "template not found: %v", err)
	}

	return s.templateModelToProto(template), nil
}

// DownloadReport - 依据: codeaudit_common.proto L948
func (s *ReportServiceImpl) DownloadReport(req *pb.DownloadReportRequest, stream pb.ReportService_DownloadReportServer) error {
	if req.GetReportId() == "" {
		return status.Error(codes.InvalidArgument, "report_id is required")
	}

	report, err := s.repo.GetReportByID(req.GetReportId())
	if err != nil {
		return status.Errorf(codes.NotFound, "report not found: %v", err)
	}

	if report.Status != "COMPLETED" {
		return status.Errorf(codes.FailedPrecondition, "report is not completed, status: %s", report.Status)
	}

	// Stream report content in chunks - 依据: 03 §5 流式传输
	chunkSize := 1024 * 1024 // 1MB chunks
	content := []byte(report.Content)

	for i := 0; i < len(content); i += chunkSize {
		end := i + chunkSize
		if end > len(content) {
			end = len(content)
		}

		chunk := &pb.ReportChunk{
			Data: content[i:end],
		}

		if err := stream.Send(chunk); err != nil {
			return status.Errorf(codes.Internal, "failed to send chunk: %v", err)
		}
	}

	return nil
}

// Helper: generateReportContent generates report based on template
// HandleTaskCompleted - 依据: ADR-006 Kafka 主路径: 消费 task.completed 触发报告生成
// 实现 service.EventHandler 接口(event_consumer.go)
func (s *ReportServiceImpl) HandleTaskCompleted(ctx context.Context, event *TaskCompletedEvent) error {
	log.Printf("Kafka主路径: task.completed 到达, task=%s, 生成报告", event.TaskID)
	_, err := s.GenerateReport(ctx, &pb.GenerateReportRequest{
		Metadata:   &pb.RequestMetadata{RequestId: fmt.Sprintf("kafka_%s_%d", event.TaskID, time.Now().UnixNano())},
		TaskId:     event.TaskID,
		TemplateId: "standard",
	})
	return err
}

func (s *ReportServiceImpl) generateReportContent(taskID string, templateName string) (string, error) {
	log.Printf("Generating report for task %s with template %s", taskID, templateName)

	// 真实聚合: 从仓储分页拉全本任务 findings（03 §5 cursor 分页），按 verdict 统计。
	// 无注入数据源时保持空报告（不臆造数字）。
	summary := map[string]int{
		"total_findings": 0, "true_positives": 0, "false_positives": 0, "not_reviewed": 0,
	}
	items := make([]map[string]interface{}, 0)
	if s.findings != nil {
		lastID := ""
		for {
			page, next, err := s.findings.List(lastID, 100, taskID, "")
			if err != nil {
				return "", fmt.Errorf("aggregate findings for report: %w", err)
			}
			for _, f := range page {
				summary["total_findings"]++
				switch strings.ToUpper(f.Verdict) {
				case "AI_VERDICT_TRUE_POSITIVE", "AI_VERDICT_CONFIRMED":
					summary["true_positives"]++
				case "AI_VERDICT_FALSE_POSITIVE":
					summary["false_positives"]++
				default:
					summary["not_reviewed"]++
				}
				items = append(items, map[string]interface{}{
					"finding_id": f.ID, "rule_id": f.RuleID, "cwe": f.CWE,
					"severity": f.Severity, "file": f.FilePath,
					"line": f.LineNumber, "verdict": f.Verdict,
					"title": f.Message, "source_raw": f.SourceRaw,
				})
			}
			if next == "" {
				break
			}
			lastID = next
		}
	}

	payload := map[string]interface{}{
		"task_id":      taskID,
		"template":     templateName,
		"generated_at": time.Now().Format(time.RFC3339),
		"summary":      summary,
		"findings":     items,
	}
	if templateName == "html" {
		// ADR-142: HTML 报告（人工可读——摘要+逐条含代码上下文），供报告中心在线查看
		return renderHTMLReport(payload, items), nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal report content: %w", err)
	}
	return string(encoded), nil
}

// renderHTMLReport — 服务端 HTML 报告（无外部依赖，go html/template 免注入按默认转义）。
// 内容全部来自真实聚合（P4）；含代码片段列（ADR-141）。
func renderHTMLReport(payload map[string]interface{}, items []map[string]interface{}) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<title>CodeAudit 审计报告</title><style>")
	b.WriteString("body{font-family:system-ui,sans-serif;margin:24px;color:#1a2233}")
	b.WriteString("table{border-collapse:collapse;width:100%;font-size:14px}")
	b.WriteString("td,th{border:1px solid #d7dce6;padding:6px 10px;text-align:left;vertical-align:top}")
	b.WriteString("th{background:#f0f3f9}pre{background:#0b1021;color:#d6e2ff;padding:8px;border-radius:4px;overflow-x:auto;font-size:12px}")
	b.WriteString(".sev-CRITICAL,.sev-HIGH{color:#c0392b;font-weight:600}.sev-MEDIUM{color:#d68910}.sev-LOW{color:#7d8a99}")
	b.WriteString("</style></head><body>")
	b.WriteString(fmt.Sprintf("<h1>CodeAudit 审计报告 — 任务 %s</h1>", htmlEsc(payload["task_id"].(string))))
	b.WriteString(fmt.Sprintf("<p>生成时间：%s ｜ 模板：%s</p>", htmlEsc(payload["generated_at"].(string)), htmlEsc(templateOf(payload))))
	sm := payload["summary"].(map[string]int)
	b.WriteString("<h2>摘要</h2><ul>")
	b.WriteString(fmt.Sprintf("<li>发现总数：%d</li><li>确认为真：%d</li><li>误报：%d</li><li>未复核：%d</li>",
		sm["total_findings"], sm["true_positives"], sm["false_positives"], sm["not_reviewed"]))
	b.WriteString("</ul><h2>发现明细</h2>")
	if len(items) == 0 {
		b.WriteString("<p>本任务无发现。</p>")
	}
	b.WriteString("<table><tr><th>严重级</th><th>CWE</th><th>规则</th><th>位置</th><th>结论</th><th>标题/说明</th><th>代码上下文</th></tr>")
	for _, it := range items {
		sev, _ := it["severity"].(string)
		b.WriteString("<tr>")
		b.WriteString(fmt.Sprintf("<td class=\"sev-%s\">%s</td>", htmlEsc(sev), htmlEsc(sev)))
		b.WriteString(fmt.Sprintf("<td>%s</td>", htmlEsc(strOf(it["cwe"]))))
		b.WriteString(fmt.Sprintf("<td>%s</td>", htmlEsc(strOf(it["rule_id"]))))
		b.WriteString(fmt.Sprintf("<td>%s:%v</td>", htmlEsc(strOf(it["file"])), it["line"]))
		b.WriteString(fmt.Sprintf("<td>%s</td>", htmlEsc(strOf(it["verdict"]))))
		b.WriteString(fmt.Sprintf("<td>%s</td>", htmlEsc(strOf(it["title"]))))
		snippet := snippetOf(it)
		b.WriteString(fmt.Sprintf("<td><pre>%s</pre></td>", snippet))
		b.WriteString("</tr>")
	}
	b.WriteString("</table></body></html>")
	return b.String()
}

func strOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func templateOf(payload map[string]interface{}) string {
	if s, ok := payload["template"].(string); ok {
		return s
	}
	return ""
}

func snippetOf(it map[string]interface{}) string {
	if s, ok := it["source_raw"].(string); ok && s != "" {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			if c, ok := obj["code"].(string); ok && c != "" {
				return c
			}
		}
		return s
	}
	return "—"
}

func htmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// Helper: modelToProto converts model.Report to proto Report (proto L1263 真实字段)
func (s *ReportServiceImpl) modelToProto(r *model.Report) *pb.Report {
	return &pb.Report{
		ReportId:    r.ID,
		TaskId:      r.TaskID,
		Format:      pb.ReportFormat(pb.ReportFormat_value[r.Format]), // ADR-142: 回读持久化格式
		Url:         fmt.Sprintf("report://%s", r.ID),                 // 内容经 DownloadReport 流式取用
		GeneratedAt: timestamppb.New(r.CreatedAt),
	}
}

// Helper: templateModelToProto converts model.ReportTemplate to proto (proto L1268 真实字段)
func (s *ReportServiceImpl) templateModelToProto(t *model.ReportTemplate) *pb.ReportTemplate {
	return &pb.ReportTemplate{
		TemplateId:  t.ID,
		Name:        t.Name,
		Description: t.Description,
	}
}
