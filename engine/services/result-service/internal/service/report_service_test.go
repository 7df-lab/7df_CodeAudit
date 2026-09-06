package service

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
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
