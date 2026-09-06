package service

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
)

// TestGenerateReportDegradationWhenKafkaUnavailable - 依据: ADR-006 gRPC降级路径
func TestGenerateReportDegradationWhenKafkaUnavailable(t *testing.T) {
	mockRepo := &MockReportRepository{
		GetReportByRequestIDFn: func(requestID string) (*model.Report, error) {
			return nil, repository.ErrNotFound
		},
		CreateReportFn: func(report *model.Report) error {
			return nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	req := &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-degradation-test",
		},
		TaskId:     "task-1",
		TemplateId: "tpl_default",
		Format:     pb.ReportFormat_REPORT_FORMAT_JSON,
	}

	resp, err := service.GenerateReport(context.Background(), req)

	// Verify degradation path works
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.Result == nil {
		t.Error("Expected result, got nil")
	}
	if resp.Result.ReportId == "" {
		t.Error("Expected report ID, got empty")
	}
	if resp.Result.ReportId != "report_task-1_req-degradation-test" {
		t.Errorf("Expected report ID 'report_task-1_req-degradation-test', got '%s'", resp.Result.ReportId)
	}
}

// TestGenerateReportDegradationConsistency - 依据: ADR-006 降级路径结果一致性
func TestGenerateReportDegradationConsistency(t *testing.T) {
	mockRepo := &MockReportRepository{
		GetReportByRequestIDFn: func(requestID string) (*model.Report, error) {
			return nil, repository.ErrNotFound
		},
		CreateReportFn: func(report *model.Report) error {
			return nil
		},
	}
	service := NewReportServiceImpl(mockRepo)

	// Create two identical requests
	req1 := &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-consistency-1",
		},
		TaskId:     "task-1",
		TemplateId: "tpl_default",
		Format:     pb.ReportFormat_REPORT_FORMAT_JSON,
	}

	req2 := &pb.GenerateReportRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-consistency-2",
		},
		TaskId:     "task-1",
		TemplateId: "tpl_default",
		Format:     pb.ReportFormat_REPORT_FORMAT_JSON,
	}

	// Call degradation path twice
	resp1, err1 := service.GenerateReport(context.Background(), req1)
	resp2, err2 := service.GenerateReport(context.Background(), req2)

	// Verify both calls succeed
	if err1 != nil {
		t.Errorf("Expected no error for req1, got %v", err1)
	}
	if err2 != nil {
		t.Errorf("Expected no error for req2, got %v", err2)
	}
	if resp1.Result == nil {
		t.Error("Expected result for req1, got nil")
	}
	if resp2.Result == nil {
		t.Error("Expected result for req2, got nil")
	}
	if resp1.Result.ReportId == "" {
		t.Error("Expected report ID for req1, got empty")
	}
	if resp2.Result.ReportId == "" {
		t.Error("Expected report ID for req2, got empty")
	}
	if resp1.Result.ReportId == resp2.Result.ReportId {
		t.Error("Expected different report IDs for different requests")
	}
}
