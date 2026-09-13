package service

// R82 锁定测试：SubmitFindingFeedback 必须持久化 feedback_type——此前 proto/model/DDL
// 三处都有该字段而实现从不读取，每次反馈分类落库为空串（误报/漏报/误级统计与
// 训练回流数据源全丢）。

import (
	"context"
	"testing"

	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"

	pb "github.com/codeaudit/proto-gen"
)

func TestSubmitFindingFeedback_PersistsFeedbackType(t *testing.T) {
	var captured *model.FindingFeedback
	mockRepo := &MockFindingRepository{
		GetFeedbackByRequestIDFn: func(requestID string) (*model.FindingFeedback, error) {
			return nil, repository.ErrNotFound
		},
		CreateFeedbackFn: func(fb *model.FindingFeedback) error {
			captured = fb
			return nil
		},
	}
	svc := NewResultServiceImpl(mockRepo)

	req := &pb.SubmitFindingFeedbackRequest{
		Metadata:     &pb.RequestMetadata{RequestId: "req-ft-r82"},
		FindingId:    "finding-1",
		FeedbackType: pb.SubmitFindingFeedbackRequest_FEEDBACK_FALSE_POSITIVE,
		Comment:      "误报",
	}
	resp, err := svc.SubmitFindingFeedback(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitFindingFeedback: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatal("want accepted")
	}
	if captured == nil {
		t.Fatal("落盘未被调用")
	}
	if captured.FeedbackType != "FEEDBACK_FALSE_POSITIVE" {
		t.Fatalf("feedback_type 落库 = %q, want FEEDBACK_FALSE_POSITIVE（分类被丢弃，R82）", captured.FeedbackType)
	}
}
