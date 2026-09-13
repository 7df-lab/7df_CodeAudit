package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MockReportRepository is a mock implementation of ReportRepository
type MockReportRepository struct {
	CreateReportFn         func(report *model.Report) error
	GetReportByIDFn        func(id string) (*model.Report, error)
	GetReportByRequestIDFn func(requestID string) (*model.Report, error)
	ListReportsFn          func(lastID string, limit int, taskID string) ([]*model.Report, string, error)
	ListTemplatesFn        func(limit int) ([]*model.ReportTemplate, error)
	GetTemplateByIDFn      func(id string) (*model.ReportTemplate, error)
	UpdateReportFn         func(report *model.Report) error
	DeleteReportFn         func(id string) error
}

// DeleteReport — ADR-212: FAILED 同键重试先除旧行的桩实现。
func (m *MockReportRepository) DeleteReport(id string) error {
	if m.DeleteReportFn != nil {
		return m.DeleteReportFn(id)
	}
	return nil
}

func (m *MockReportRepository) UpdateReport(report *model.Report) error {
	if m.UpdateReportFn != nil {
		return m.UpdateReportFn(report)
	}
	return nil
}

func (m *MockReportRepository) CreateReport(report *model.Report) error {
	if m.CreateReportFn != nil {
		return m.CreateReportFn(report)
	}
	return nil
}

func (m *MockReportRepository) GetReportByID(id string) (*model.Report, error) {
	if m.GetReportByIDFn != nil {
		return m.GetReportByIDFn(id)
	}
	return nil, repository.ErrNotFound
}

func (m *MockReportRepository) GetReportByRequestID(requestID string) (*model.Report, error) {
	if m.GetReportByRequestIDFn != nil {
		return m.GetReportByRequestIDFn(requestID)
	}
	return nil, repository.ErrNotFound
}

func (m *MockReportRepository) ListReports(lastID string, limit int, taskID string) ([]*model.Report, string, error) {
	if m.ListReportsFn != nil {
		return m.ListReportsFn(lastID, limit, taskID)
	}
	return nil, "", nil
}

func (m *MockReportRepository) ListTemplates(limit int) ([]*model.ReportTemplate, error) {
	if m.ListTemplatesFn != nil {
		return m.ListTemplatesFn(limit)
	}
	return nil, nil
}

func (m *MockReportRepository) GetTemplateByID(id string) (*model.ReportTemplate, error) {
	if m.GetTemplateByIDFn != nil {
		return m.GetTemplateByIDFn(id)
	}
	return nil, repository.ErrNotFound
}

// TestGenerateReportIdempotent - 依据: codeaudit_common.proto L943
func TestGenerateReportIdempotent(t *testing.T) {
	callCount := 0
	mockRepo := &MockReportRepository{
		GetReportByRequestIDFn: func(requestID string) (*model.Report, error) {
			if callCount > 0 {
				return &model.Report{
					ID:        "report_task-1_req-1",
					TaskID:    "task-1",
					Template:  "tpl_default",
					Format:    "REPORT_FORMAT_JSON",
					Status:    "COMPLETED",
					Url:       "https://storage.codeaudit.local/reports/report_task-1_req-1",
					RequestID: "req-1",
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}, nil
			}
			return nil, repository.ErrNotFound
		},
		CreateReportFn: func(report *model.Report) error {
			callCount++
			return nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-1",
		},
		TaskId:     "task-1",
		TemplateId: "tpl_default",
		Format:     pb.ReportFormat_REPORT_FORMAT_JSON,
	}

	resp1, err := service.GenerateReport(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp1.Result == nil {
		t.Error("Expected result, got nil")
	}

	resp2, err := service.GenerateReport(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp2.Result == nil {
		t.Error("Expected result (idempotent replay), got nil")
	}
	if resp2.Result.ReportId != "report_task-1_req-1" {
		t.Errorf("Expected report ID 'report_task-1_req-1', got '%s'", resp2.Result.ReportId)
	}
}

// TestGenerateReportMissingMetadata - 依据: 03 §2 幂等三态
func TestGenerateReportMissingMetadata(t *testing.T) {
	mockRepo := &MockReportRepository{}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.GenerateReportRequest{
		TaskId:     "task-1",
		TemplateId: "tpl_default",
	}

	_, err := service.GenerateReport(context.Background(), req)
	if err == nil {
		t.Error("Expected error for missing metadata, got nil")
	}
}

// TestGetReport - 依据: codeaudit_common.proto L944
func TestGetReport(t *testing.T) {
	mockRepo := &MockReportRepository{
		GetReportByIDFn: func(id string) (*model.Report, error) {
			return &model.Report{
				ID:        "report-1",
				TaskID:    "task-1",
				Template:  "tpl_default",
				Format:    "REPORT_FORMAT_JSON",
				Status:    "COMPLETED",
				Url:       "https://storage.codeaudit.local/reports/report-1",
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}, nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.GetReportRequest{
		ReportId: "report-1",
	}

	resp, err := service.GetReport(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.ReportId != "report-1" {
		t.Errorf("Expected report ID 'report-1', got '%s'", resp.ReportId)
	}
}

// TestListReportsWithCursor - 依据: 03 §5 cursor 分页
func TestListReportsWithCursor(t *testing.T) {
	mockRepo := &MockReportRepository{
		ListReportsFn: func(lastID string, limit int, taskID string) ([]*model.Report, string, error) {
			return []*model.Report{
				{ID: "report-1", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
				{ID: "report-2", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
			}, "report-2", nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.ListReportsRequest{
		TaskId: "task-1",
		Pagination: &pb.PaginationRequest{
			PageSize: 20,
		},
	}

	resp, err := service.ListReports(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if len(resp.Reports) != 2 {
		t.Errorf("Expected 2 reports, got %d", len(resp.Reports))
	}
	if resp.Pagination == nil {
		t.Error("Expected pagination, got nil")
	}
	if resp.Pagination.NextCursor == "" {
		t.Error("Expected next cursor, got empty")
	}
}

// TestListTemplates - 依据: codeaudit_common.proto L946
func TestListTemplates(t *testing.T) {
	mockRepo := &MockReportRepository{
		ListTemplatesFn: func(limit int) ([]*model.ReportTemplate, error) {
			return []*model.ReportTemplate{
				{ID: "tpl_default", Name: "Default Report", Description: "Standard audit report"},
				{ID: "tpl_executive", Name: "Executive Summary", Description: "High-level summary"},
			}, nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.ListTemplatesRequest{
		Pagination: &pb.PaginationRequest{
			PageSize: 20,
		},
	}

	resp, err := service.ListTemplates(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if len(resp.Templates) != 2 {
		t.Errorf("Expected 2 templates, got %d", len(resp.Templates))
	}
}

// TestGetTemplate - 依据: codeaudit_common.proto L947
func TestGetTemplate(t *testing.T) {
	mockRepo := &MockReportRepository{
		GetTemplateByIDFn: func(id string) (*model.ReportTemplate, error) {
			return &model.ReportTemplate{
				ID:          "tpl_default",
				Name:        "Default Report",
				Description: "Standard audit report",
			}, nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.GetTemplateRequest{
		TemplateId: "tpl_default",
	}

	resp, err := service.GetTemplate(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.TemplateId != "tpl_default" {
		t.Errorf("Expected template ID 'tpl_default', got '%s'", resp.TemplateId)
	}
}

// ADR-212 回归①：ADR-135 允许 FAILED 报告同键重试，但重试沿用同一确定性 ID
// 对主键裸 INSERT 必冲突——每次重试恒 500，"允许重试"从未真正可达。
// 修复=先除旧行再重建，同键重试幂等于同一 report_id。
func TestGenerateReport_FailedReportRetry_SameID(t *testing.T) {
	var deleted []string
	var createdIDs []string
	repo := &MockReportRepository{
		GetReportByRequestIDFn: func(requestID string) (*model.Report, error) {
			// 首次返回已存在的 FAILED 报告（ADR-135：失败报告不参与重放）
			if requestID == "req-r" {
				return &model.Report{ID: "report_t_req-r", TaskID: "t", Status: "FAILED", RequestID: requestID}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		DeleteReportFn: func(id string) error {
			deleted = append(deleted, id)
			return nil
		},
		CreateReportFn: func(report *model.Report) error {
			createdIDs = append(createdIDs, report.ID)
			return nil
		},
	}
	s := NewReportServiceImpl(repo)
	resp, err := s.GenerateReport(context.Background(), &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-r"}, TaskId: "t",
	})
	if err != nil {
		t.Fatalf("retry must succeed post-fix (pre-fix: PK conflict 500): %v", err)
	}
	if resp.GetResult().GetReportId() != "report_t_req-r" {
		t.Fatalf("retry must keep deterministic report id, got %s", resp.GetResult().GetReportId())
	}
	if len(deleted) != 1 || deleted[0] != "report_t_req-r" {
		t.Fatalf("FAILED row must be cleared before rebuild: %v", deleted)
	}
	if len(createdIDs) != 1 || createdIDs[0] != "report_t_req-r" {
		t.Fatalf("unexpected creates: %v", createdIDs)
	}
}

// ADR-212 回归②：Kafka 主路径 request_id 确定化——原 UnixNano 唯一键使
// 重投递（rebalance/重放）每次都生成新报告；重复消费必须幂等重放。
func TestHandleTaskCompleted_Redelivery_Idempotent(t *testing.T) {
	queried := []string{}
	repo := &MockReportRepository{
		GetReportByRequestIDFn: func(requestID string) (*model.Report, error) {
			queried = append(queried, requestID)
			if requestID == "kafka_t-1" {
				return &model.Report{ID: "report_t-1_kafka_t-1", TaskID: "t-1", Status: "COMPLETED", RequestID: requestID}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		CreateReportFn: func(report *model.Report) error { return nil },
	}
	s := NewReportServiceImpl(repo)
	for i := 0; i < 2; i++ {
		if err := s.HandleTaskCompleted(context.Background(), &TaskCompletedEvent{TaskID: "t-1"}); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if len(queried) != 2 || queried[0] != "kafka_t-1" || queried[1] != "kafka_t-1" {
		t.Fatalf("request_id must be deterministic kafka_<task>, got %v", queried)
	}
}

// R42（2026-09-11 修复批次）: 代码片段列必须逐字段 htmlEsc——finding 的 code
// 字段来自被扫源码（攻击者可控），未转义直写 <pre> 即存储型 XSS（web 报告窗口渲染）。
func TestRenderHTMLReport_SnippetEscaped(t *testing.T) {
	items := []map[string]interface{}{{
		"severity": "HIGH", "cwe": "CWE-79", "rule_id": "bandit.B101", "file": "a.py",
		"line": 1, "verdict": "AI_VERDICT_TRUE_POSITIVE", "title": "t",
		"source_raw": `{"code": "<script>alert(1)</script>"}`,
	}}
	payload := map[string]interface{}{
		"task_id": "t-r42", "generated_at": "2026-09-11",
		"summary": map[string]int{"total_findings": 1, "true_positives": 0, "false_positives": 0, "not_reviewed": 1},
	}
	html := renderHTMLReport(payload, items)
	if strings.Contains(html, "<script>alert") {
		t.Fatal("report snippet rendered unescaped — stored XSS via finding code field (R42)")
	}
	if !strings.Contains(html, "&lt;script&gt;alert") {
		t.Fatal("escaped form expected in rendered report")
	}
}

// R44（2026-09-11 修复批次）: 已归档报告的读路径必须返回真实归档 Url——
// 恒造 report:// 伪协议令客户端拿到不可取回的地址（归档信息被丢弃）。
func TestGetReport_ReturnsArchivedUrl(t *testing.T) {
	mockRepo := &MockReportRepository{
		GetReportByIDFn: func(id string) (*model.Report, error) {
			return &model.Report{
				ID: "report-arch", TaskID: "task-arch", Format: "REPORT_FORMAT_PDF",
				Status: "COMPLETED", Url: "https://storage.internal/reports/report-arch",
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}, nil
		},
	}
	resp, err := NewReportServiceImpl(mockRepo).GetReport(context.Background(),
		&pb.GetReportRequest{ReportId: "report-arch"})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if resp.GetUrl() != "https://storage.internal/reports/report-arch" {
		t.Fatalf("archived url expected, got %q (report:// pseudo-url = archived url dropped, R44)", resp.GetUrl())
	}
}

// R66（2026-09-12 待办收尾）：内容生成失败必须如实报错（FailedPrecondition）——
// 此前 success 返回令编排发 done:report 阶段绿勾（FAILED 报告伪装成功）。
func TestGenerateReport_ContentFailureHonest(t *testing.T) {
	repo := &MockReportRepository{
		GetReportByRequestIDFn: func(string) (*model.Report, error) { return nil, fmt.Errorf("not found") },
		DeleteReportFn:         func(string) error { return nil },
		CreateReportFn:         func(r *model.Report) error { return nil },
	}
	s := NewReportServiceImpl(repo)
	_, err := s.GenerateReport(context.Background(), &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-r66"}, TaskId: "", // 空 task_id? 需真失败——用缺任务触发内容失败
	})
	_ = err // 占位：空 task_id 在参数校验即 400——真正内容失败用不可聚合任务
	// 构造内容失败：ListFindings 报错路径经 mock repo 不可达（service 层 generateReportContent 依赖聚合）
	// 用 handler 级注入太重；此处锁"FAILED 行为经 FakeRepo 模拟内容错误"不可行时，
	// 以行为锚：直接调用私有路径不可取（test-gates 口径），改锁公开行为——见下
	s.SetFindingRepository(&failingFindingsRepo{})
	_, err = s.GenerateReport(context.Background(), &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-r66b"}, TaskId: "t-r66",
	})
	if err == nil {
		t.Fatal("content-failure path must return error (R66) — got success")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition (content failure honest, R66), got %v", err)
	}
	if !strings.Contains(err.Error(), "report_id=report_t-r66") {
		t.Fatalf("error should carry report id for retry-same-id, got %v", err)
	}
}

// failingFindingsRepo — List 恒错（注入内容聚合失败）。
type failingFindingsRepo struct{ repository.FindingRepository }

func (f *failingFindingsRepo) List(cursor string, limit int, taskID, q string) ([]*model.Finding, string, error) {
	return nil, "", fmt.Errorf("injected aggregate failure")
}
