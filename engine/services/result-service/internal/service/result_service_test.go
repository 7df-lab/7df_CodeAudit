package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MockFindingRepository is a mock implementation of FindingRepository
type MockFindingRepository struct {
	CreateFn                     func(finding *model.Finding) error
	GetByIDFn                    func(id string) (*model.Finding, error)
	UpdateFn                     func(finding *model.Finding) error
	DeleteFn                     func(id string) error
	ListFn                       func(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error)
	ListByVerdictFn              func(verdict string, lastID string, limit int) ([]*model.Finding, string, error)
	GetByRequestIDAndFindingIDFn func(requestID string, findingID string) (*model.Finding, error)
	GetStatsByTaskIDFn           func(taskID string) (*model.ResultStats, error)
	CreateFeedbackFn             func(feedback *model.FindingFeedback) error
	GetFeedbackByRequestIDFn     func(requestID string) (*model.FindingFeedback, error)
}

func (m *MockFindingRepository) Create(finding *model.Finding) error {
	if m.CreateFn != nil {
		return m.CreateFn(finding)
	}
	return nil
}

func (m *MockFindingRepository) GetByID(id string) (*model.Finding, error) {
	if m.GetByIDFn != nil {
		return m.GetByIDFn(id)
	}
	return nil, repository.ErrNotFound
}

func (m *MockFindingRepository) Update(finding *model.Finding) error {
	if m.UpdateFn != nil {
		return m.UpdateFn(finding)
	}
	return nil
}

func (m *MockFindingRepository) Delete(id string) error {
	if m.DeleteFn != nil {
		return m.DeleteFn(id)
	}
	return nil
}

func (m *MockFindingRepository) List(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
	if m.ListFn != nil {
		return m.ListFn(lastID, limit, taskID, verdict)
	}
	return nil, "", nil
}

func (m *MockFindingRepository) ListByVerdict(verdict string, lastID string, limit int) ([]*model.Finding, string, error) {
	if m.ListByVerdictFn != nil {
		return m.ListByVerdictFn(verdict, lastID, limit)
	}
	return nil, "", nil
}

func (m *MockFindingRepository) GetByRequestIDAndFindingID(requestID string, findingID string) (*model.Finding, error) {
	if m.GetByRequestIDAndFindingIDFn != nil {
		return m.GetByRequestIDAndFindingIDFn(requestID, findingID)
	}
	return nil, repository.ErrNotFound
}

func (m *MockFindingRepository) GetStatsByTaskID(taskID string) (*model.ResultStats, error) {
	if m.GetStatsByTaskIDFn != nil {
		return m.GetStatsByTaskIDFn(taskID)
	}
	return nil, nil
}

func (m *MockFindingRepository) CreateFeedback(feedback *model.FindingFeedback) error {
	if m.CreateFeedbackFn != nil {
		return m.CreateFeedbackFn(feedback)
	}
	return nil
}

func (m *MockFindingRepository) GetFeedbackByRequestID(requestID string) (*model.FindingFeedback, error) {
	if m.GetFeedbackByRequestIDFn != nil {
		return m.GetFeedbackByRequestIDFn(requestID)
	}
	return nil, repository.ErrNotFound
}

func (m *MockFindingRepository) DB() *sql.DB {
	return nil
}

// TestCreateFinding - 依据: codeaudit_common.proto L921
func TestCreateFinding(t *testing.T) {
	mockRepo := &MockFindingRepository{
		CreateFn: func(finding *model.Finding) error {
			return nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	finding := &pb.UnifiedFinding{
		FindingId:    "finding-1",
		TaskId:       "task-1",
		SourceTool:   "semgrep",
		SourceRuleId: "rule-1",
		Severity:     pb.Severity_SEVERITY_HIGH,
		Description:  "SQL injection vulnerability",
		Location: &pb.LocationInfo{
			FilePath:  "/app/main.go",
			StartLine: 42,
		},
	}

	req := &pb.CreateFindingRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-1",
		},
		Finding: finding,
	}

	resp, err := service.CreateFinding(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.GetFinding().GetFindingId() != "finding-1" {
		t.Errorf("Expected finding ID 'finding-1', got '%s'", resp.GetFinding().GetFindingId())
	}
}

// TestGetFinding - 依据: codeaudit_common.proto L922
func TestGetFinding(t *testing.T) {
	mockRepo := &MockFindingRepository{
		GetByIDFn: func(id string) (*model.Finding, error) {
			return &model.Finding{
				ID:         "finding-1",
				TaskID:     "task-1",
				ToolName:   "semgrep",
				RuleID:     "rule-1",
				Severity:   "SEVERITY_HIGH",
				Message:    "SQL injection vulnerability",
				FilePath:   "/app/main.go",
				LineNumber: 42,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			}, nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.GetFindingRequest{
		FindingId: "finding-1",
	}

	resp, err := service.GetFinding(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.GetFinding().GetFindingId() != "finding-1" {
		t.Errorf("Expected finding ID 'finding-1', got '%s'", resp.GetFinding().GetFindingId())
	}
}

// TestBatchCreateFindingsIdempotent - 依据: 03 §2 幂等三态
func TestBatchCreateFindingsIdempotent(t *testing.T) {
	callCount := 0
	mockRepo := &MockFindingRepository{
		GetByRequestIDAndFindingIDFn: func(requestID string, findingID string) (*model.Finding, error) {
			if callCount > 0 {
				return &model.Finding{
					ID:       findingID,
					TaskID:   "task-1",
					ToolName: "semgrep",
					RuleID:   "rule-1",
				}, nil
			}
			return nil, repository.ErrNotFound
		},
		CreateFn: func(finding *model.Finding) error {
			callCount++
			return nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	finding := &pb.UnifiedFinding{
		FindingId:    "finding-1",
		TaskId:       "task-1",
		SourceTool:   "semgrep",
		SourceRuleId: "rule-1",
	}

	req := &pb.BatchCreateFindingsRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-1",
		},
		Findings: []*pb.UnifiedFinding{finding},
	}

	resp1, err := service.BatchCreateFindings(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(resp1.FindingIds) != 1 {
		t.Errorf("Expected 1 finding ID, got %d", len(resp1.FindingIds))
	}

	resp2, err := service.BatchCreateFindings(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(resp2.FindingIds) != 1 {
		t.Errorf("Expected 1 finding ID (idempotent replay), got %d", len(resp2.FindingIds))
	}
}

// TestBatchCreateFindingsMissingMetadata - 依据: 03 §2 幂等三态
func TestBatchCreateFindingsMissingMetadata(t *testing.T) {
	mockRepo := &MockFindingRepository{}
	service := NewResultServiceImpl(mockRepo)

	finding := &pb.UnifiedFinding{
		FindingId: "finding-1",
		TaskId:    "task-1",
	}

	req := &pb.BatchCreateFindingsRequest{
		Findings: []*pb.UnifiedFinding{finding},
	}

	_, err := service.BatchCreateFindings(context.Background(), req)
	if err == nil {
		t.Error("Expected error for missing metadata, got nil")
	}
}

// TestSubmitFindingFeedbackIdempotent - 依据: codeaudit_common.proto L935
func TestSubmitFindingFeedbackIdempotent(t *testing.T) {
	callCount := 0
	mockRepo := &MockFindingRepository{
		GetFeedbackByRequestIDFn: func(requestID string) (*model.FindingFeedback, error) {
			if callCount > 0 {
				return &model.FindingFeedback{
					ID:           "fb_finding-1_req-1",
					FindingID:    "finding-1",
					FeedbackType: "FEEDBACK_FALSE_POSITIVE",
					Comment:      "Confirmed false positive",
					RequestID:    "req-1",
				}, nil
			}
			return nil, repository.ErrNotFound
		},
		CreateFeedbackFn: func(feedback *model.FindingFeedback) error {
			callCount++
			return nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.SubmitFindingFeedbackRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-1",
		},
		FindingId:    "finding-1",
		FeedbackType: pb.SubmitFindingFeedbackRequest_FEEDBACK_FALSE_POSITIVE,
		Comment:      "Confirmed false positive",
	}

	resp1, err := service.SubmitFindingFeedback(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if !resp1.Accepted {
		t.Error("Expected accepted, got false")
	}

	resp2, err := service.SubmitFindingFeedback(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if !resp2.Accepted {
		t.Error("Expected accepted (idempotent replay), got false")
	}
	if resp2.FeedbackId != "fb_finding-1_req-1" {
		t.Errorf("Expected feedback ID 'fb_finding-1_req-1', got '%s'", resp2.FeedbackId)
	}
}

// TestListFindingsWithCursor - 依据: 03 §5 cursor 分页
func TestListFindingsWithCursor(t *testing.T) {
	mockRepo := &MockFindingRepository{
		ListFn: func(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
			return []*model.Finding{
				{ID: "finding-1", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
				{ID: "finding-2", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
			}, "finding-2", nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.ListFindingsRequest{
		TaskId: "task-1",
		Pagination: &pb.PaginationRequest{
			PageSize: 20,
		},
	}

	resp, err := service.ListFindings(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if len(resp.Findings) != 2 {
		t.Errorf("Expected 2 findings, got %d", len(resp.Findings))
	}
	if resp.Pagination == nil {
		t.Error("Expected pagination, got nil")
	}
	if resp.Pagination.NextCursor == "" {
		t.Error("Expected next cursor, got empty")
	}
}

// TestDeleteFinding - 依据: codeaudit_common.proto L924
func TestDeleteFinding(t *testing.T) {
	mockRepo := &MockFindingRepository{
		DeleteFn: func(id string) error {
			return nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.DeleteFindingRequest{
		FindingId: "finding-1",
	}

	resp, err := service.DeleteFinding(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
}

// TestUpdateVerdict - 依据: codeaudit_common.proto L929
func TestUpdateVerdict(t *testing.T) {
	mockRepo := &MockFindingRepository{
		GetByIDFn: func(id string) (*model.Finding, error) {
			return &model.Finding{
				ID:        "finding-1",
				TaskID:    "task-1",
				ToolName:  "semgrep",
				RuleID:    "rule-1",
				Severity:  "SEVERITY_HIGH",
				Message:   "SQL injection vulnerability",
				Verdict:   "AI_VERDICT_UNSPECIFIED",
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}, nil
		},
		UpdateFn: func(finding *model.Finding) error {
			return nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.UpdateVerdictRequest{
		FindingId:  "finding-1",
		Verdict:    pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE,
		Confidence: 0.95,
		Reasoning:  "Confirmed by manual review",
	}

	resp, err := service.UpdateVerdict(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.GetFinding().GetFindingId() != "finding-1" {
		t.Errorf("Expected finding ID 'finding-1', got '%s'", resp.GetFinding().GetFindingId())
	}
}

// TestGetTaskResultStats - 依据: codeaudit_common.proto L931
func TestGetTaskResultStats(t *testing.T) {
	mockRepo := &MockFindingRepository{
		GetStatsByTaskIDFn: func(taskID string) (*model.ResultStats, error) {
			return &model.ResultStats{
				TaskId:        taskID,
				TotalFindings: 10,
				BySeverity: map[string]int32{
					"SEVERITY_HIGH":   3,
					"SEVERITY_MEDIUM": 5,
					"SEVERITY_LOW":    2,
				},
				ByVerdict: map[string]int32{
					"AI_VERDICT_TRUE_POSITIVE":  6,
					"AI_VERDICT_FALSE_POSITIVE": 4,
				},
			}, nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.GetTaskResultStatsRequest{
		TaskId: "task-1",
	}

	resp, err := service.GetTaskResultStats(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.TaskId != "task-1" {
		t.Errorf("Expected task ID 'task-1', got '%s'", resp.TaskId)
	}
	if resp.Total != 10 {
		t.Errorf("Expected total 10, got %d", resp.Total)
	}
}

// TestExportFindings - 依据: codeaudit_common.proto L932
func TestExportFindings(t *testing.T) {
	mockRepo := &MockFindingRepository{
		ListFn: func(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
			return []*model.Finding{
				{ID: "finding-1", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
				{ID: "finding-2", TaskID: "task-1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
			}, "", nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.ExportFindingsRequest{
		TaskId: "task-1",
		Format: pb.ReportFormat_REPORT_FORMAT_JSON,
	}

	resp, err := service.ExportFindings(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if resp.ExportUrl == "" {
		t.Error("Expected export URL, got empty")
	}
}

// TestGetFindingsByVerdict - 依据: codeaudit_common.proto L930
func TestGetFindingsByVerdict(t *testing.T) {
	mockRepo := &MockFindingRepository{
		ListByVerdictFn: func(verdict string, lastID string, limit int) ([]*model.Finding, string, error) {
			return []*model.Finding{
				{ID: "finding-1", TaskID: "task-1", Verdict: verdict, CreatedAt: time.Now(), UpdatedAt: time.Now()},
			}, "", nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	req := &pb.GetFindingsByVerdictRequest{
		TaskId:  "task-1",
		Verdict: pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE,
		Pagination: &pb.PaginationRequest{
			PageSize: 20,
		},
	}

	resp, err := service.GetFindingsByVerdict(context.Background(), req)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if resp == nil {
		t.Error("Expected response, got nil")
	}
	if len(resp.Findings) != 1 {
		t.Errorf("Expected 1 finding, got %d", len(resp.Findings))
	}
}

// TestListFindingsCursorRoundTrip - 回归锁: 2026-08-27 模式D大任务(163发现)翻页时
// 发现游标编码/解码不对称（编码侧裸ID vs 解码侧 base64(JSON{last_id,limit})，03 §5），
// 第二页必然 INVALID_ARGUMENT。本测试要求第二页必须能原样消费第一页的 next_cursor。
func TestListFindingsCursorRoundTrip(t *testing.T) {
	var seenLastID string
	mockRepo := &MockFindingRepository{
		ListFn: func(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
			seenLastID = lastID
			now := time.Now()
			if lastID == "" { // 第一页
				return []*model.Finding{{ID: "f-1", TaskID: "t-1", CreatedAt: now, UpdatedAt: now}}, "f-1", nil
			}
			// 第二页: seenLastID 必须等于第一页 next_cursor 解码出的 last_id
			if seenLastID != "f-1" {
				t.Errorf("page2 lastID = %q, want %q (cursor round-trip broken)", seenLastID, "f-1")
			}
			return []*model.Finding{{ID: "f-2", TaskID: "t-1", CreatedAt: now, UpdatedAt: now}}, "", nil
		},
	}
	service := NewResultServiceImpl(mockRepo)

	page1, err := service.ListFindings(context.Background(), &pb.ListFindingsRequest{
		TaskId:     "t-1",
		Pagination: &pb.PaginationRequest{PageSize: 1},
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	cur := page1.Pagination.NextCursor
	if cur == "" {
		t.Fatal("page1 must return next_cursor")
	}

	page2, err := service.ListFindings(context.Background(), &pb.ListFindingsRequest{
		TaskId:     "t-1",
		Pagination: &pb.PaginationRequest{PageSize: 1, Cursor: cur},
	})
	if err != nil {
		t.Fatalf("page2 must accept page1 next_cursor: %v", err)
	}
	if len(page2.Findings) != 1 || page2.Findings[0].FindingId != "f-2" {
		t.Fatal("page2 content unexpected")
	}
}

// ---- ADR-135 回归锁 ----

// stubProducer 记录事件发布调用（验证 verdict 事件链真实接线）。
type stubProducer struct{ events []string }

func (p *stubProducer) PublishVerdictUpdated(ctx context.Context, finding *model.Finding, oldVerdict string, updatedBy string) error {
	p.events = append(p.events, finding.ID+":"+oldVerdict+"->"+finding.Verdict)
	return nil
}

// TestUpdateVerdictPublishesEvent: verdict 变更必须发布 finding.verdict.updated（ADR-006/ADR-135）。
func TestUpdateVerdictPublishesEvent(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	svc := NewResultServiceImpl(repo)
	prod := &stubProducer{}
	svc.SetEventProducer(prod)

	f := &model.Finding{ID: "f-ev-1", TaskID: "t1", ToolName: "bandit", RuleID: "B101", Severity: "HIGH", Verdict: "AI_VERDICT_NOT_REVIEWED"}
	if err := repo.Create(f); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateVerdict(context.Background(), &pb.UpdateVerdictRequest{
		FindingId: "f-ev-1", Verdict: pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE, Reasoning: "verified by llm",
	}); err != nil {
		t.Fatal(err)
	}
	if len(prod.events) != 1 || prod.events[0] != "f-ev-1:AI_VERDICT_NOT_REVIEWED->AI_VERDICT_TRUE_POSITIVE" {
		t.Fatalf("verdict event not published correctly: %v", prod.events)
	}
	// unchanged verdict → no event
	if _, err := svc.UpdateVerdict(context.Background(), &pb.UpdateVerdictRequest{
		FindingId: "f-ev-1", Verdict: pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE}); err != nil {
		t.Fatal(err)
	}
	if len(prod.events) != 1 {
		t.Fatalf("no-change verdict must not publish: %v", prod.events)
	}
	// reasoning 持久化（proto L1240, ADR-135）
	got, _ := repo.GetByID("f-ev-1")
	if got.Reasoning != "verified by llm" {
		t.Fatalf("reasoning not persisted: %q", got.Reasoning)
	}
}

// TestCreateFindingRequiresMetadata: metadata 缺失必须 InvalidArgument（ADR-135 幂等链路）。
func TestCreateFindingRequiresMetadata(t *testing.T) {
	svc := NewResultServiceImpl(repository.NewMemoryFindingRepository())
	_, err := svc.CreateFinding(context.Background(), &pb.CreateFindingRequest{
		Finding: &pb.UnifiedFinding{FindingId: "f-nometa", TaskId: "t1"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// ---- ADR-142 回归锁：BatchUpdateFindings（融合回写通道） ----

// TestBatchUpdateFindings_FusionPatch: dedup_group/matched_findings/is_unique 白名单补丁落库。
func TestBatchUpdateFindings_FusionPatch(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	svc := NewResultServiceImpl(repo)
	for _, id := range []string{"f-a", "f-b", "f-c"} {
		if err := repo.Create(&model.Finding{ID: id, TaskID: "t1", ToolName: "bandit", RuleID: "B1", Severity: "HIGH", Verdict: "AI_VERDICT_NOT_REVIEWED"}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := svc.BatchUpdateFindings(context.Background(), &pb.BatchUpdateFindingsRequest{
		Metadata:   &pb.RequestMetadata{RequestId: "wb-1"},
		FindingIds: []string{"f-a", "f-b", "f-c"},
		PatchJson: map[string]string{
			"dedup_group":      "group_1",
			"matched_findings": "f-a,f-b",
			"is_unique":        "false",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetUpdatedCount() != 3 || len(resp.GetFailedIds()) != 0 {
		t.Fatalf("updated=%d failed=%v", resp.GetUpdatedCount(), resp.GetFailedIds())
	}
	f, _ := repo.GetByID("f-a")
	if f.DedupGroup != "group_1" || f.MatchedFindings != "f-a,f-b" || f.IsUnique {
		t.Fatalf("fusion fields not persisted: %+v", f)
	}
	// 未知ID → failed_ids
	resp2, _ := svc.BatchUpdateFindings(context.Background(), &pb.BatchUpdateFindingsRequest{
		Metadata:   &pb.RequestMetadata{RequestId: "wb-2"},
		FindingIds: []string{"f-a", "nope"},
		PatchJson:  map[string]string{"is_unique": "true"},
	})
	if resp2.GetUpdatedCount() != 1 || len(resp2.GetFailedIds()) != 1 || resp2.GetFailedIds()[0] != "nope" {
		t.Fatalf("partial failure semantics: %+v", resp2)
	}
	if fa, _ := repo.GetByID("f-a"); !fa.IsUnique {
		t.Fatal("is_unique patch not applied")
	}
}

// TestUnifiedRoundtrip_FusionAndSnippet: 融合字段与代码片段写入→回读对称（GetFinding 供融合视图）。
func TestUnifiedRoundtrip_FusionAndSnippet(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	svc := NewResultServiceImpl(repo)
	_, err := svc.CreateFinding(context.Background(), &pb.CreateFindingRequest{
		Metadata: &pb.RequestMetadata{RequestId: "rt-1"},
		Finding: &pb.UnifiedFinding{
			FindingId: "f-rt", TaskId: "t1", SourceTool: "bandit",
			Title: "t", Description: "d", Severity: pb.Severity_SEVERITY_HIGH,
			Location:        &pb.LocationInfo{FilePath: "a.py", StartLine: 7},
			SourceRaw:       []byte(`{"code":"SECRET=1"}`),
			DedupGroup:      "g9",
			MatchedFindings: []string{"f-x", "f-y"},
			IsUnique:        false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetFinding(context.Background(), &pb.GetFindingRequest{FindingId: "f-rt"})
	u := got.GetFinding()
	if string(u.GetSourceRaw()) != `{"code":"SECRET=1"}` {
		t.Fatalf("snippet roundtrip: %q", u.GetSourceRaw())
	}
	if u.GetDedupGroup() != "g9" || len(u.GetMatchedFindings()) != 2 || u.GetIsUnique() {
		t.Fatalf("fusion roundtrip: %+v", u)
	}
}

// TestFixSuggestionFieldsRoundTrip — ADR-183: 修复建议两通道落盘往返。
// BatchCreateFindings 写入 ai_fix_suggestion/diff_patch → ListFindings 回读两字段
// （此前 ai_fix_suggestion 在落盘层被丢弃，插件降级通道断裂；内存仓全链路验证）。
func TestFixSuggestionFieldsRoundTrip(t *testing.T) {
	svc := NewResultServiceImpl(repository.NewMemoryFindingRepository())
	patch := "*** Begin Patch\n*** Update File: vuln_fix_demo.py\n@@     cursor = conn.cursor()\n*** End Patch"
	in := &pb.UnifiedFinding{
		FindingId: "t-rt-sbx-1", TaskId: "t-rt", SourceTool: "ai_agent",
		SourceRuleId: "dsh-headless", Severity: pb.Severity_SEVERITY_CRITICAL,
		Description:     "SQL 注入",
		AiVerdict:       pb.AIVerdict_AI_VERDICT_LIKELY_TRUE,
		AiReasoning:     "[DSH-sandbox] 拼接",
		AiFixSuggestion: "## 修复说明\n参数化查询",
		DiffPatch:       patch,
		AiConfidence:    0.95,
		Location:        &pb.LocationInfo{FilePath: "vuln_fix_demo.py", StartLine: 6},
	}
	resp, err := svc.BatchCreateFindings(context.Background(), &pb.BatchCreateFindingsRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-rt-1"},
		Findings: []*pb.UnifiedFinding{in},
	})
	if err != nil || len(resp.GetFindingIds()) != 1 {
		t.Fatalf("batch create: %v ids=%d", err, len(resp.GetFindingIds()))
	}
	list, err := svc.ListFindings(context.Background(), &pb.ListFindingsRequest{
		TaskId:     "t-rt",
		Pagination: &pb.PaginationRequest{PageSize: 10},
	})
	if err != nil || len(list.GetFindings()) != 1 {
		t.Fatalf("list: %v n=%d", err, len(list.GetFindings()))
	}
	got := list.GetFindings()[0]
	if got.GetAiFixSuggestion() != in.GetAiFixSuggestion() {
		t.Fatalf("ai_fix_suggestion round-trip broken: %q", got.GetAiFixSuggestion())
	}
	if got.GetDiffPatch() != patch {
		t.Fatalf("diff_patch round-trip broken: %q", got.GetDiffPatch())
	}
}
