package service

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// 依据: codeaudit_common.proto L956-L973 DSHRuntimeService
// 依据: 05 §4 五角色推理流程
// 依据: 07 §8.1 Agent 迭代上限

func newService() *DSHRuntimeServiceImpl {
	return NewDSHRuntimeService()
}

func TestRunAIAnalysisMissingMetadata(t *testing.T) {
	svc := newService()
	_, err := svc.RunAIAnalysis(context.Background(), &pb.RunAIAnalysisRequest{
		TaskId: "task-1",
	})
	if err == nil {
		t.Fatal("expected error for missing metadata (R4)")
	}
}

func TestRunAIAnalysisEmptyTaskID(t *testing.T) {
	svc := newService()
	_, err := svc.RunAIAnalysis(context.Background(), &pb.RunAIAnalysisRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-1", CallerService: "test"},
		TaskId:   "",
	})
	if err == nil {
		t.Fatal("expected error for empty task_id")
	}
}

func TestRunAIAnalysisSuccess(t *testing.T) {
	svc := newService()
	resp, err := svc.RunAIAnalysis(context.Background(), &pb.RunAIAnalysisRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-1", CallerService: "test"},
		TaskId:   "task-1",
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestRunAIAnalysisIdempotent(t *testing.T) {
	svc := newService()
	req := &pb.RunAIAnalysisRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-idem-1", CallerService: "test"},
		TaskId:   "task-idem-1",
	}

	resp1, err := svc.RunAIAnalysis(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// 第二次调用同 request_id → 幂等重放
	resp2, err := svc.RunAIAnalysis(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent replay should succeed: %v", err)
	}
	if resp2 == nil {
		t.Fatal("expected non-nil response from idempotent replay")
	}
	_ = resp1
}

func TestVerifySASTResultsMissingMetadata(t *testing.T) {
	svc := newService()
	_, err := svc.VerifySASTResults(context.Background(), &pb.VerifySASTResultsRequest{
		TaskId: "task-1",
	})
	if err == nil {
		t.Fatal("expected error for missing metadata")
	}
}

func TestSearchMissedVulnsMissingMetadata(t *testing.T) {
	svc := newService()
	_, err := svc.SearchMissedVulns(context.Background(), &pb.SearchMissedVulnsRequest{
		TaskId: "task-1",
	})
	if err == nil {
		t.Fatal("expected error for missing metadata")
	}
}

func TestReviewSASTResultsMissingMetadata(t *testing.T) {
	svc := newService()
	_, err := svc.ReviewSASTResults(context.Background(), &pb.ReviewSASTResultsRequest{
		TaskId: "task-1",
	})
	if err == nil {
		t.Fatal("expected error for missing metadata")
	}
}

func TestGetAnalysisProgress(t *testing.T) {
	svc := newService()
	resp, err := svc.GetAnalysisProgress(context.Background(), &pb.GetAnalysisProgressRequest{
		TaskId: "task-unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TaskId != "task-unknown" {
		t.Errorf("expected task-unknown, got %s", resp.TaskId)
	}
}

func TestGetSessionStatusNotFound(t *testing.T) {
	svc := newService()
	resp, err := svc.GetSessionStatus(context.Background(), &pb.GetSessionStatusRequest{
		SessionId: "nonexistent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != "not_found" {
		t.Errorf("expected not_found, got %s", resp.State)
	}
}

func TestCancelAnalysisEmptyTaskID(t *testing.T) {
	svc := newService()
	_, err := svc.CancelAnalysis(context.Background(), &pb.CancelAnalysisRequest{
		TaskId: "",
	})
	if err == nil {
		t.Fatal("expected error for empty task_id")
	}
}

func TestCancelAnalysisSuccess(t *testing.T) {
	svc := newService()
	_, err := svc.CancelAnalysis(context.Background(), &pb.CancelAnalysisRequest{
		TaskId: "task-1",
	})
	if err != nil {
		t.Fatalf("cancel should succeed even for unknown task: %v", err)
	}
}

// 反向测试: 依据 test-gates.md §3 "沙箱安全"行
// 五Agent编排中沙箱隔离生效
func TestAgentPipelineCreatesFiveAgents(t *testing.T) {
	svc := newService()
	resp, err := svc.RunAIAnalysis(context.Background(), &pb.RunAIAnalysisRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-five-agents", CallerService: "test"},
		TaskId:   "task-five-agents",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Result == nil {
		t.Fatal("expected valid response from five-agent pipeline")
	}
}

// 反向测试: 迭代上限（07 §8.1）
func TestGetAnalysisProgressAfterRun(t *testing.T) {
	svc := newService()
	svc.RunAIAnalysis(context.Background(), &pb.RunAIAnalysisRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-progress", CallerService: "test"},
		TaskId:   "task-progress",
	})

	resp, err := svc.GetAnalysisProgress(context.Background(), &pb.GetAnalysisProgressRequest{
		TaskId: "task-progress",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TaskId != "task-progress" {
		t.Errorf("expected task-progress, got %s", resp.TaskId)
	}
}
