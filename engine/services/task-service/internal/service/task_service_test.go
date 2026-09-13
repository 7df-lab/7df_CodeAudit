package service

// 修复回归锁（ADR-131）：假成功 RPC 真实化 / 幂等三态 / 自动重试耗尽→DEAD /
// 状态机单一权威 / 进度统计。依据: 03 §2、04 §1、proto L174/L177/L880-L881。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newSvc(t *testing.T) *TaskServiceImpl {
	t.Helper()
	// 下游端口不监听：编排快速失败，用于驱动重试/终态路径
	t.Setenv("CODEAUDIT_SAST_ADAPTER_ADDR", "localhost:59990")
	t.Setenv("CODEAUDIT_DSH_RUNTIME_ADDR", "localhost:59991")
	t.Setenv("CODEAUDIT_RESULT_ADDR", "localhost:59992")
	return NewTaskService()
}

func createTask(t *testing.T, s *TaskServiceImpl, id string, mode pb.ScanMode) *pb.ScanTask {
	t.Helper()
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: id},
		ProjectId: "p-" + id,
		ScanMode:  mode,
	})
	if err != nil {
		t.Fatalf("CreateScanTask: %v", err)
	}
	return task
}

// mustStart — ADR-171: 审批流废除，CREATED → RUNNING 经 StartTask 直达（必须成功）。
func mustStart(t *testing.T, s *TaskServiceImpl, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.StartTask(ctx, &pb.StartTaskRequest{TaskId: id}); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
}

func waitForStatus(t *testing.T, s *TaskServiceImpl, id string, want pb.TaskStatus, timeout time.Duration) *pb.ScanTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := s.GetScanTask(context.Background(), &pb.GetScanTaskRequest{TaskId: id})
		if err != nil {
			t.Fatalf("GetScanTask: %v", err)
		}
		if task.GetStatus() == want {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach %s in %v", id, want, timeout)
	return nil
}

func TestCreateScanTask_IdempotentThreeState(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()

	first, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-1"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	replay, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-1"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
	})
	if err != nil {
		t.Fatalf("replay should succeed: %v", err)
	}
	if replay.GetTaskId() != first.GetTaskId() {
		t.Fatalf("replay returned different task")
	}
	// 同键异体 → ALREADY_EXISTS（03 §2），不得重放旧任务
	_, err = s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-1"},
		ProjectId: "p2-different",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("same key different body: want AlreadyExists, got %v", err)
	}
}

func TestReportStageComplete_ThreeState(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-stage", pb.ScanMode_SCAN_MODE_AI_ONLY)

	md := &pb.RequestMetadata{RequestId: "stage-req-1"}
	ok1 := &pb.ReportStageCompleteRequest{Metadata: md, TaskId: "req-stage", StageId: "analyze",
		OutputRefs: map[string]string{"cpg": "/tmp/cpg.json"}}
	if _, err := s.ReportStageComplete(ctx, ok1); err != nil {
		t.Fatalf("first report: %v", err)
	}
	// 同键同体 → 幂等回放
	if _, err := s.ReportStageComplete(ctx, ok1); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	// 同键异体（不同 output_refs）→ ALREADY_EXISTS
	ok2 := &pb.ReportStageCompleteRequest{Metadata: md, TaskId: "req-stage", StageId: "ai",
		OutputRefs: map[string]string{"x": "y"}}
	if _, err := s.ReportStageComplete(ctx, ok2); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different body: want AlreadyExists, got %v", err)
	}
	// 未知任务 → NOT_FOUND（不再是假成功）
	bad := &pb.ReportStageCompleteRequest{Metadata: &pb.RequestMetadata{RequestId: "stage-req-2"},
		TaskId: "no-such-task", StageId: "analyze"}
	if _, err := s.ReportStageComplete(ctx, bad); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown task: want NotFound, got %v", err)
	}
	// 阶段状态真实落账
	task, _ := s.GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: "req-stage"})
	found := false
	for _, st := range task.GetStages() {
		if st.GetStageId() == "analyze" && st.GetStatus() == pb.StageStatus_STAGE_STATUS_COMPLETED {
			found = true
			if st.GetMetadata()["cpg"] != "/tmp/cpg.json" {
				t.Fatalf("output_refs not persisted: %v", st.GetMetadata())
			}
		}
	}
	if !found {
		t.Fatalf("stage not COMPLETED: %v", task.GetStages())
	}
}

// TestRegisterStages_AIEnhancedSast — 模式D AI增强SAST（ADR-186）阶段预注册：
// sast→ai(验证)→fusion→report，与模式C 同集（验证事件经 stageEventStageID 映射到 ai 位）。
func TestRegisterStages_AIEnhancedSast(t *testing.T) {
	s := newSvc(t)
	s.mu.Lock()
	task := &pb.ScanTask{TaskId: "t-d-enh", Status: pb.TaskStatus_TASK_STATUS_CREATED,
		ScanMode: pb.ScanMode_SCAN_MODE_AI_ENHANCED_SAST}
	s.registerStagesLocked(task)
	s.mu.Unlock()
	want := []string{"sast", "ai", "fusion", "report"}
	got := make([]string, 0, len(task.GetStages()))
	for _, st := range task.GetStages() {
		got = append(got, st.GetStageId())
		// ADR-212①: 注册阶段必须就地初始化 Metadata——ReportStageComplete 写
		// output_refs 时 nil map 赋值 panic（变异自检 M2 锚点）
		if st.GetMetadata() == nil {
			t.Fatalf("stage %s registered without Metadata map (nil-map panic on output_refs)", st.GetStageId())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("stages want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stage[%d] want %s, got %s", i, want[i], got[i])
		}
	}
}

func TestReportStageFailed_RecordsError(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-stagef", pb.ScanMode_SCAN_MODE_SAST_REVIEW)

	_, err := s.ReportStageFailed(ctx, &pb.ReportStageFailedRequest{
		Metadata: &pb.RequestMetadata{RequestId: "sf-1"}, TaskId: "req-stagef",
		StageId: "sast", ErrorMessage: "bandit crashed",
	})
	if err != nil {
		t.Fatalf("ReportStageFailed: %v", err)
	}
	task, _ := s.GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: "req-stagef"})
	for _, st := range task.GetStages() {
		if st.GetStageId() == "sast" {
			if st.GetStatus() != pb.StageStatus_STAGE_STATUS_FAILED || st.GetErrorMessage() != "bandit crashed" {
				t.Fatalf("stage failed state not recorded: %+v", st)
			}
			return
		}
	}
	t.Fatalf("stage sast not found in %v", task.GetStages())
}

func TestOrchestrationFailure_AutoRetryExhaustedToDead(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	if _, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-retry"},
		ProjectId: "p-retry",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/rt"}, // 有路径：编排真实启动
	}); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s, "req-retry")
	// 下游全部不可达：3 次执行（首次+2 重试）后应到 DEAD（proto L174/L177）
	// R48: CreateScanTask 现返回克隆——断言须读最新快照（活引用反模式已除）
	res := waitForStatus(t, s, "req-retry", pb.TaskStatus_TASK_STATUS_DEAD, 30*time.Second)
	if int(res.GetRetryCount()) != maxAutoRetries {
		t.Fatalf("retry_count = %d, want %d", res.GetRetryCount(), maxAutoRetries)
	}
	if res.GetErrorMessage() == "" {
		t.Fatalf("error_message should be persisted")
	}
}

// TestStartTask_MissingProjectPath_FailsHonest（ADR-148）：
// 无 project_path 且项目配置亦无 → 明确 FAILED + 报错，不再回退 samples 也不空跑重试。
func TestStartTask_MissingProjectPath_FailsHonest(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-nopath", pb.ScanMode_SCAN_MODE_AI_ONLY)
	_, err := s.StartTask(ctx, &pb.StartTaskRequest{TaskId: "req-nopath"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	task, _ := s.GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: "req-nopath"})
	if task.GetStatus() != pb.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("want FAILED, got %s", task.GetStatus())
	}
	if !strings.Contains(task.GetErrorMessage(), "project_path") {
		t.Fatalf("honest message missing: %q", task.GetErrorMessage())
	}
}

func TestFailTask_RetryableRequeues_ThenDead(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-fail", pb.ScanMode_SCAN_MODE_AI_ONLY)
	// ADR-171: QUEUED 稳态随审批流废除（仅自动重试瞬态）——CREATED 上 FailTask 必须被拒
	if _, err := s.FailTask(ctx, &pb.FailTaskRequest{TaskId: "req-fail", ErrorMessage: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("FailTask on CREATED must be FailedPrecondition (statemachine 单一权威), got %v", err)
	}
}

func TestCancelScanTask_StateMachineAuthority(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-cancel", pb.ScanMode_SCAN_MODE_AI_ONLY)

	// CREATED 可取消（04 §1 任何状态可取消）
	if _, err := s.CancelScanTask(ctx, &pb.CancelScanTaskRequest{TaskId: "req-cancel"}); err != nil {
		t.Fatalf("cancel created task: %v", err)
	}
	// CANCELLED 是终态：再取消 → FailedPrecondition
	if _, err := s.CancelScanTask(ctx, &pb.CancelScanTaskRequest{TaskId: "req-cancel"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-cancel must fail, got %v", err)
	}
}

func TestGetTaskProgress_ComputedFromStages(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	createTask(t, s, "req-prog", pb.ScanMode_SCAN_MODE_AI_ONLY)

	p, err := s.GetTaskProgress(ctx, &pb.GetTaskProgressRequest{TaskId: "req-prog"})
	if err != nil {
		t.Fatalf("GetTaskProgress: %v", err)
	}
	if p.GetOverallPercent() != 0 {
		t.Fatalf("initial progress = %v, want 0", p.GetOverallPercent())
	}
	if _, err := s.UpdateStageStatus(ctx, &pb.UpdateStageStatusRequest{
		TaskId: "req-prog", StageId: "analyze", Status: pb.StageStatus_STAGE_STATUS_COMPLETED}); err != nil {
		t.Fatalf("UpdateStageStatus: %v", err)
	}
	p, _ = s.GetTaskProgress(ctx, &pb.GetTaskProgressRequest{TaskId: "req-prog"})
	if p.GetOverallPercent() != 100 {
		t.Fatalf("progress after 1/1 completed = %v, want 100", p.GetOverallPercent())
	}
}

func TestGetTaskContext_NotFoundBeforeCompletion(t *testing.T) {
	s := newSvc(t)
	if _, err := s.GetTaskContext(context.Background(), &pb.GetTaskContextRequest{TaskId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestListScanTasks_StableOrderAndPagination(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	for _, id := range []string{"req-l3", "req-l1", "req-l2"} {
		createTask(t, s, id, pb.ScanMode_SCAN_MODE_AI_ONLY)
	}
	resp, err := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		Pagination: &pb.PaginationRequest{PageSize: 2}})
	if err != nil {
		t.Fatalf("ListScanTasks: %v", err)
	}
	if len(resp.GetTasks()) != 2 {
		t.Fatalf("page size 2: got %d", len(resp.GetTasks()))
	}
	if !resp.GetPagination().GetHasNext() || resp.GetPagination().GetNextCursor() == "" {
		t.Fatalf("expected next cursor")
	}
	resp2, _ := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		Pagination: &pb.PaginationRequest{PageSize: 2, Cursor: resp.GetPagination().GetNextCursor()}})
	if len(resp2.GetTasks()) != 1 {
		t.Fatalf("second page wrong: %v", resp2.GetTasks())
	}
	// 稳定序断言：两次全量拉取顺序一致，且页1+页2 拼接 == 全量顺序（03 §5 稳定游标）
	full1, _ := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{Pagination: &pb.PaginationRequest{PageSize: 100}})
	full2, _ := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{Pagination: &pb.PaginationRequest{PageSize: 100}})
	var seq1, seq2, concat []string
	for _, tk := range full1.GetTasks() {
		seq1 = append(seq1, tk.GetTaskId())
	}
	for _, tk := range full2.GetTasks() {
		seq2 = append(seq2, tk.GetTaskId())
	}
	for _, tk := range resp.GetTasks() {
		concat = append(concat, tk.GetTaskId())
	}
	for _, tk := range resp2.GetTasks() {
		concat = append(concat, tk.GetTaskId())
	}
	if strings.Join(seq1, ",") != strings.Join(seq2, ",") {
		t.Fatalf("ordering unstable between calls: %v vs %v", seq1, seq2)
	}
	if strings.Join(seq1, ",") != strings.Join(concat, ",") {
		t.Fatalf("pagination does not preserve stable order: %v vs %v", seq1, concat)
	}
	// 非法游标 → INVALID_ARGUMENT（03 §5）
	_, err = s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		Pagination: &pb.PaginationRequest{Cursor: "not-a-number"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid cursor: want InvalidArgument, got %v", err)
	}
}

// ---- ADR-149b 回归锁：阶段重置与终态语义 ----

// TestResetStagesLocked: （重）启动前阶段看板归零——清除 FAILED 残留与错误信息。
func TestResetStagesLocked(t *testing.T) {
	s := newSvc(t)
	now := timestamppb.Now()
	task := &pb.ScanTask{TaskId: "t", Status: pb.TaskStatus_TASK_STATUS_QUEUED, Stages: []*pb.TaskStage{
		{StageId: "sast", Status: pb.StageStatus_STAGE_STATUS_FAILED, ErrorMessage: "old error", StartedAt: now, CompletedAt: now},
		{StageId: "ai", Status: pb.StageStatus_STAGE_STATUS_COMPLETED, StartedAt: now, CompletedAt: now},
	}}
	s.mu.Lock()
	s.resetStagesLocked(task)
	s.mu.Unlock()
	for _, st := range task.GetStages() {
		if st.GetStatus() != pb.StageStatus_STAGE_STATUS_PENDING || st.GetErrorMessage() != "" ||
			st.GetStartedAt() != nil || st.GetCompletedAt() != nil {
			t.Fatalf("stage %s not reset: %+v", st.GetStageId(), st)
		}
	}
}

// TestFinalizeStages_SkipsNeverStarted: 失败时已启动=FAILED，未启动=SKIPPED（不再一律 FAILED）。
func TestFinalizeStages_SkipsNeverStarted(t *testing.T) {
	s := newSvc(t)
	task := &pb.ScanTask{TaskId: "t", Status: pb.TaskStatus_TASK_STATUS_RUNNING, Stages: []*pb.TaskStage{
		{StageId: "ran", Status: pb.StageStatus_STAGE_STATUS_RUNNING, StartedAt: timestamppb.Now()},
		{StageId: "never", Status: pb.StageStatus_STAGE_STATUS_PENDING},
	}}
	s.mu.Lock()
	s.finalizeStagesLocked(task, context.DeadlineExceeded)
	s.mu.Unlock()
	got := map[string]pb.StageStatus{}
	for _, st := range task.GetStages() {
		got[st.GetStageId()] = st.GetStatus()
	}
	if got["ran"] != pb.StageStatus_STAGE_STATUS_FAILED {
		t.Fatalf("started stage should be FAILED: %v", got)
	}
	if got["never"] != pb.StageStatus_STAGE_STATUS_SKIPPED {
		t.Fatalf("never-started stage should be SKIPPED: %v", got)
	}
}

// TestListScanTasks_ProjectAndModeFilter — ADR-160：契约 L1108-1112 的 project_id
// 与 filter（scan_mode/status, EQ/NEQ, AND/OR）过滤真实生效；未知字段诚实拒绝。
func TestListScanTasks_ProjectAndModeFilter(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	mk := func(id, project string, mode pb.ScanMode) {
		if _, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
			Metadata: &pb.RequestMetadata{RequestId: id}, ProjectId: project, ScanMode: mode}); err != nil {
			t.Fatalf("CreateScanTask %s: %v", id, err)
		}
	}
	mk("fx-a1", "p-demo", pb.ScanMode_SCAN_MODE_AI_ONLY)
	mk("fx-b1", "p-demo", pb.ScanMode_SCAN_MODE_TRADITIONAL_FIRST)
	mk("fx-b2", "p-e2e", pb.ScanMode_SCAN_MODE_TRADITIONAL_FIRST)

	// project_id 过滤
	resp, err := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{ProjectId: "p-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTasks()) != 2 {
		t.Fatalf("project filter: got %d, want 2", len(resp.GetTasks()))
	}

	// filter scan_mode EQ
	resp, err = s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		Filter: &pb.FilterRequest{Conditions: []*pb.FilterCondition{
			{Field: "scan_mode", Operator: pb.FilterOperator_FILTER_OPERATOR_EQ, Value: "SCAN_MODE_TRADITIONAL_FIRST"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTasks()) != 2 {
		t.Fatalf("mode EQ filter: got %d, want 2", len(resp.GetTasks()))
	}

	// 组合：project_id + mode NEQ
	resp, err = s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		ProjectId: "p-demo",
		Filter: &pb.FilterRequest{Conditions: []*pb.FilterCondition{
			{Field: "scan_mode", Operator: pb.FilterOperator_FILTER_OPERATOR_NEQ, Value: "SCAN_MODE_AI_ONLY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTasks()) != 1 || resp.GetTasks()[0].GetTaskId() != "fx-b1" {
		t.Fatalf("combined filter: got %d tasks", len(resp.GetTasks()))
	}

	// 未知字段 → InvalidArgument（诚实拒绝而非静默忽略）
	if _, err := s.ListScanTasks(ctx, &pb.ListScanTasksRequest{
		Filter: &pb.FilterRequest{Conditions: []*pb.FilterCondition{
			{Field: "hack", Operator: pb.FilterOperator_FILTER_OPERATOR_EQ, Value: "x"}}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown field: want InvalidArgument, got %v", err)
	}
}

// ---- 任务执行日志（ADR-167）：幂等追加 / 游标增量 / 环形上限 / 流转史 ----

func TestAppendTaskLog_IdempotentReplay(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "log-1", pb.ScanMode_SCAN_MODE_AI_ONLY)
	req := &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: "log-1-r1"},
		TaskId:   "log-1", Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO,
		Source: "sandbox", Message: "sandbox created am-abc",
	}
	r1, err := s.AppendTaskLog(context.Background(), req)
	if err != nil {
		t.Fatalf("AppendTaskLog: %v", err)
	}
	r2, err := s.AppendTaskLog(context.Background(), req) // 同键重放
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r1.Entry.GetLogId() != r2.Entry.GetLogId() {
		t.Fatalf("idempotent replay must return same entry: %s vs %s", r1.Entry.GetLogId(), r2.Entry.GetLogId())
	}
	resp, err := s.GetTaskLogs(context.Background(), &pb.GetTaskLogsRequest{TaskId: "log-1"})
	if err != nil {
		t.Fatalf("GetTaskLogs: %v", err)
	}
	if len(resp.Logs) != 1 {
		t.Fatalf("replay must not duplicate: %d logs", len(resp.Logs))
	}
}

func TestGetTaskLogs_AfterCursorAndOrder(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "log-2", pb.ScanMode_SCAN_MODE_AI_ONLY)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
			Metadata: &pb.RequestMetadata{RequestId: fmt.Sprintf("log-2-r%d", i)},
			TaskId:   "log-2", Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO,
			Source: "dsh-runtime", Message: fmt.Sprintf("step %d", i),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	first, err := s.GetTaskLogs(ctx, &pb.GetTaskLogsRequest{TaskId: "log-2"})
	if err != nil || len(first.Logs) != 3 {
		t.Fatalf("initial fetch: %v %d", err, len(first.GetLogs()))
	}
	cursor := first.Logs[0].GetLogId()
	inc, err := s.GetTaskLogs(ctx, &pb.GetTaskLogsRequest{TaskId: "log-2", AfterLogId: cursor})
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(inc.Logs) != 2 || inc.Logs[0].GetMessage() != "step 1" {
		t.Fatalf("cursor fetch must skip consumed entries: %d", len(inc.Logs))
	}
}

func TestTaskLog_RingCapAndTransitionHistory(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	// 带 project_path：StartTask 才能通过校验进入 RUNNING
	if _, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "log-3"},
		ProjectId: "p-log-3",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/log-3"},
	}); err != nil {
		t.Fatal(err)
	}
	// 流转史：create 直接落 CREATED；ADR-171 审批流废除 → StartTask 产生一条（CREATED→RUNNING）
	mustStart(t, s, "log-3")
	base, err := s.GetTaskLogs(ctx, &pb.GetTaskLogsRequest{TaskId: "log-3"})
	if err != nil {
		t.Fatalf("GetTaskLogs: %v", err)
	}
	if len(base.Logs) != 1 {
		t.Fatalf("transition history expected 1 log (start), got %d", len(base.Logs))
	}
	sources := map[string]bool{}
	for _, e := range base.Logs {
		sources[e.GetSource()] = true
	}
	if !sources["task"] {
		t.Fatalf("transition logs must carry source=task: %v", sources)
	}
	// 超限环形丢弃：塞满后仍可追加且数量封顶
	for i := 0; i < 600; i++ {
		_, _ = s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
			Metadata: &pb.RequestMetadata{RequestId: fmt.Sprintf("log-3-flood-%d", i)},
			TaskId:   "log-3", Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO,
			Source: "dsh-runtime", Message: fmt.Sprintf("flood %d", i),
		})
	}
	capped, err := s.GetTaskLogs(ctx, &pb.GetTaskLogsRequest{TaskId: "log-3"})
	if err != nil {
		t.Fatalf("capped fetch: %v", err)
	}
	if len(capped.Logs) > 500 {
		t.Fatalf("ring cap violated: %d", len(capped.Logs))
	}
}

func TestAppendTaskLog_Validation(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{TaskId: "x", Message: "m"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing request_id must be InvalidArgument, got %v", err)
	}
	createTask(t, s, "log-4", pb.ScanMode_SCAN_MODE_AI_ONLY)
	if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: "log-4-r"}, TaskId: "log-4", Message: "m"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: "log-4-r2"}, TaskId: "no-such", Message: "m"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown task must be NotFound, got %v", err)
	}
}

// TestStageRecorder_LiveRunningAndDone — ADR-181 回归锁：阶段事件实时流转——
// 首事件置 RUNNING（含时间戳），done:<id> 事件即时置 COMPLETED，未启动阶段不被
// 误动；这是"时间线中间态"（人类反馈 2026-09-02）的服务端权威行为。
func TestStageRecorder_LiveRunningAndDone(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "req-stage-live", pb.ScanMode_SCAN_MODE_TRADITIONAL_FIRST)
	rec := s.stageRecorder("req-stage-live")

	rec("scans", "submitting") // 首事件 → sast RUNNING
	rec("fusion", "submitting FuseResults")
	task, err := s.GetScanTask(context.Background(), &pb.GetScanTaskRequest{TaskId: "req-stage-live"})
	if err != nil {
		t.Fatalf("GetScanTask: %v", err)
	}
	byID := map[string]*pb.TaskStage{}
	for _, st := range task.GetStages() {
		byID[st.GetStageId()] = st
	}
	if st := byID["sast"]; st == nil || st.GetStatus() != pb.StageStatus_STAGE_STATUS_RUNNING || st.GetStartedAt() == nil {
		t.Fatalf("sast must be RUNNING with StartedAt, got %+v", st)
	}
	if st := byID["fusion"]; st == nil || st.GetStatus() != pb.StageStatus_STAGE_STATUS_RUNNING {
		t.Fatalf("fusion must be RUNNING (S7/fusion 键), got %+v", st)
	}
	if st := byID["report"]; st != nil && st.GetStatus() != pb.StageStatus_STAGE_STATUS_PENDING {
		t.Fatalf("report must stay PENDING (or absent) before any event, got %+v", st)
	}

	rec("done:sast", "5 findings from 1 tools") // 实时完成
	rec("done:sast", "idempotent replay")       // 已终态幂等跳过
	task, _ = s.GetScanTask(context.Background(), &pb.GetScanTaskRequest{TaskId: "req-stage-live"})
	for _, st := range task.GetStages() {
		if st.GetStageId() == "sast" {
			if st.GetStatus() != pb.StageStatus_STAGE_STATUS_COMPLETED || st.GetCompletedAt() == nil {
				t.Fatalf("sast must be COMPLETED with CompletedAt, got %+v", st)
			}
		}
	}
}

// TestRunOrchestration_RecorderWired — ADR-181 回归锁：Recorder 必须真实挂到
// RunRequest（此前创建了却漏挂——阶段事件从未到达看板，时间线全程静止）。
// 判据：下游不可达的真实编排里，analyze 阶段必须带 StartedAt（经历过 RUNNING；
// dial 失败走降级完成语义），而漏挂时 finalize 只会落"从未启动"的 SKIPPED（无 StartedAt）。
func TestRunOrchestration_RecorderWired(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	if _, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-recwire"},
		ProjectId: "p-recwire",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/rt"}, // 有路径：编排真实启动
	}); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s, "req-recwire")
	waitForStatus(t, s, "req-recwire", pb.TaskStatus_TASK_STATUS_DEAD, 30*time.Second)
	task, _ := s.GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: "req-recwire"})
	var analyze *pb.TaskStage
	for _, st := range task.GetStages() {
		if st.GetStageId() == "analyze" {
			analyze = st
		}
	}
	if analyze == nil {
		t.Fatal("analyze stage missing")
	}
	if analyze.GetStartedAt() == nil || analyze.GetStatus() == pb.StageStatus_STAGE_STATUS_SKIPPED {
		t.Fatalf("analyze must have started via wired recorder, got %+v", analyze)
	}
}

// ADR-212 回归：已注册阶段（StartTask 路径 registerStagesLocked）不带 Metadata 时，
// ReportStageComplete 写 output_refs = "assignment to entry in nil map" panic
// （grpc-go 无内建 recover=杀进程；既有 ThreeState 用例未 Start 任务，走的是
// findOrInsert 的插入分支，从未覆盖本路径）。修复=注册即初始化+防御式补齐。
func TestReportStageComplete_OutputRefsOnRegisteredStage(t *testing.T) {
	s := newSvc(t)
	s.mu.Lock()
	task := &pb.ScanTask{TaskId: "t-reg-meta", Status: pb.TaskStatus_TASK_STATUS_CREATED,
		ScanMode: pb.ScanMode_SCAN_MODE_AI_ONLY}
	s.tasks[task.TaskId] = task
	s.registerStagesLocked(task) // analyze/ai/report：与 StartTask 同一注册路径
	s.mu.Unlock()

	if _, err := s.ReportStageComplete(context.Background(),
		&pb.ReportStageCompleteRequest{
			Metadata: &pb.RequestMetadata{RequestId: "stg-reg-1"},
			TaskId:   "t-reg-meta", StageId: "analyze",
			OutputRefs: map[string]string{"cpg": "/tmp/cpg.json"},
		}); err != nil {
		t.Fatalf("report on registered stage must not fail (pre-fix: nil map panic): %v", err)
	}
	s.mu.RLock()
	var meta map[string]string
	for _, st := range task.GetStages() {
		if st.GetStageId() == "analyze" {
			meta = st.GetMetadata()
		}
	}
	s.mu.RUnlock()
	if meta["cpg"] != "/tmp/cpg.json" {
		t.Fatalf("output_refs not persisted on registered stage: %v", meta)
	}
}

// R50（2026-09-11 修复批次）: 幂等键在而原条目已被环形丢弃——回空壳回执（原 log_id），
// 不再回落追加新条目（同 request_id 二次入账破坏幂等语义）。
func TestAppendTaskLog_ReplayAfterRingEviction_NoRefill(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "log-ev", pb.ScanMode_SCAN_MODE_AI_ONLY)
	req := &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: "log-ev-r1"},
		TaskId:   "log-ev", Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO,
		Source: "sandbox", Message: "will be ring-evicted",
	}
	r1, err := s.AppendTaskLog(context.Background(), req)
	if err != nil {
		t.Fatalf("AppendTaskLog: %v", err)
	}
	s.logs["log-ev"] = nil // 模拟环形丢弃（cap 500 之后该条目出局）
	r2, err := s.AppendTaskLog(context.Background(), req)
	if err != nil {
		t.Fatalf("replay after eviction: %v", err)
	}
	if r2.Entry.GetLogId() != r1.Entry.GetLogId() {
		t.Fatalf("replay must return original log_id %s, got %s (R50)", r1.Entry.GetLogId(), r2.Entry.GetLogId())
	}
	if len(s.logs["log-ev"]) != 0 {
		t.Fatalf("replay must not append a new entry (R50): %d entries", len(s.logs["log-ev"]))
	}
}

// R57（2026-09-11 报障修复）: 阶段事件 msg 必须落 TaskStage.metadata——此前 msg 参数
// 被整体丢弃，降级信息（"[降级]" 前缀）与进度行只进日志不进看板，前端无从渲染。
func TestStageRecorder_MsgAndDegradedLandInMetadata(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "req-r57", pb.ScanMode_SCAN_MODE_AI_ONLY)
	rec := s.stageRecorder("req-r57")

	// aiStage — 锁内取 ai 阶段快照（三段断言共用；未注册时返回 nil）
	aiStage := func() *pb.TaskStage {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, g := range s.tasks["req-r57"].GetStages() {
			if g.GetStageId() == "ai" {
				return g
			}
		}
		return nil
	}

	rec("ai", "submitting RunAIAnalysis")
	st := aiStage()
	if st == nil {
		t.Fatal("ai stage not registered")
	}
	if st.GetMetadata()["message"] != "submitting RunAIAnalysis" {
		t.Fatalf("progress msg must land in metadata, got %q", st.GetMetadata()["message"])
	}

	rec("ai", "[降级] RuleScan 兜底（沙箱路径不可达）——全部发现需人工复核")
	st = aiStage()
	if st.GetMetadata()["degraded"] != "true" {
		t.Fatalf("degraded marker must land in metadata, got %v", st.GetMetadata())
	}

	rec("done:ai", "4a+4b settled: verified=0 missed=3")
	st = aiStage()
	if st.GetStatus() != pb.StageStatus_STAGE_STATUS_COMPLETED {
		t.Fatalf("done event must complete stage, got %s", st.GetStatus())
	}
	if st.GetMetadata()["message"] != "4a+4b settled: verified=0 missed=3" {
		t.Fatalf("done msg must land in metadata, got %q", st.GetMetadata()["message"])
	}
	if st.GetMetadata()["degraded"] != "true" {
		t.Fatal("degraded marker must survive done message overwrite")
	}
}

// R64/D5finding.created 事件构造纯函数锁定——载荷字段对齐
// storage eventPayload 消费映射（task_id/finding_id/severity/created_by），高危过滤口径
// 与消费端 HIGH/CRITICAL 阈值一致。
func TestBuildFindingCreatedEvent_Shape(t *testing.T) {
	msg := buildFindingCreatedEvent("t-1", "user-9", pb.Severity_SEVERITY_HIGH, "f-1")
	if msg.Topic != "finding.created" {
		t.Fatalf("topic: %s", msg.Topic)
	}
	var payload map[string]any
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"task_id", "finding_id", "severity", "created_by"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("payload missing %s: %v", k, payload)
		}
	}
	if payload["severity"] != "SEVERITY_HIGH" || payload["created_by"] != "user-9" {
		t.Fatalf("payload values: %v", payload)
	}
	found := false
	for _, h := range msg.Headers {
		if h.Key == "event_type" && string(h.Value) == "finding.created" {
			found = true
		}
	}
	if !found {
		t.Fatal("event_type header missing (ADR-212⑦)")
	}
}

// R67（2026-09-12 待办收尾·取消传播）：CancelScanTask 必须取消在途编排——
// 原实现只改状态不通知编排协程：不可达下游的重试循环继续跑（阻塞 RPC 逐次超时），
// 且被取消任务可能被下一次自动重试拉回 RUNNING（状态覆盖守卫之外的窗口）。
// blockingExecutor — R75: 可阻塞编排桩——Execute 挂起直到 ctx 被取消，模拟
// ADR-191 撤超时后的在途长调用（取消是唯一打断机制）。
type blockingExecutor struct{ entered chan struct{} }

func (b *blockingExecutor) Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error) {
	if b.entered != nil {
		close(b.entered) // 在途已确立
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// R75: 取消传播真验证——原用例的三个盲区（快速失败重试循环绕开阻塞场景/不断言
// cancel 被消费/泄漏断言在废功 delete 下平凡通过）使传播失效不可测（伪绿）。
func TestCancelTask_PropagatesToOrchestration(t *testing.T) {
	s := newSvc(t)
	ex := &blockingExecutor{entered: make(chan struct{})}
	s.orch = ex // R75 seam：注入可阻塞编排
	ctx := context.Background()
	if _, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-cancel-r67"},
		ProjectId: "p-cancel",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/rt"},
	}); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s, "req-cancel-r67")
	<-ex.entered // 编排已在途（阻塞于 Execute）
	// 注册表必须仍有本次取消器——原缺陷：runOrchestration 入口 delete 自废注册
	s.mu.RLock()
	_, registered := s.cancels["req-cancel-r67"]
	s.mu.RUnlock()
	if !registered {
		t.Fatal("cancels 注册表无条目：取消器被编排入口注销（R67 废功回潮，取消传播断裂）")
	}
	if _, err := s.CancelScanTask(ctx, &pb.CancelScanTaskRequest{TaskId: "req-cancel-r67"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 取消必须打断在途 Execute：协程退出（注册表清空）且状态稳定 CANCELLED
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.RLock()
		_, leaked := s.cancels["req-cancel-r67"]
		st := s.tasks["req-cancel-r67"].GetStatus()
		s.mu.RUnlock()
		if !leaked && st == pb.TaskStatus_TASK_STATUS_CANCELLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消未打断在途编排（leaked=%v status=%s）——传播链断裂", leaked, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// R68（2026-09-12 待办收尾）：idem 表重启遗忘后同键重放不得重置既有任务——
// task_id=request_id + 指纹随任务落库（_idem_fp），重放返回原任务；异体返回 AlreadyExists。
func TestCreateScanTask_PostRestartReplayNoReset(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	mkReq := func(mode pb.ScanMode) *pb.CreateScanTaskRequest {
		return &pb.CreateScanTaskRequest{
			Metadata: &pb.RequestMetadata{RequestId: "req-r68"}, ProjectId: "p-r68",
			ScanMode: mode, Config: map[string]string{"project_path": "/tmp/rt"},
		}
	}
	if _, err := s.CreateScanTask(ctx, mkReq(pb.ScanMode_SCAN_MODE_AI_ONLY)); err != nil {
		t.Fatal(err)
	}
	// 模拟重启遗忘：清 idem 表，保留任务实体（PG 侧在测试为内存，语义同）
	s.mu.Lock()
	delete(s.idem, "req-r68")
	s.mu.Unlock()
	if _, err := s.StartTask(ctx, &pb.StartTaskRequest{TaskId: "req-r68"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.CreateScanTask(ctx, mkReq(pb.ScanMode_SCAN_MODE_AI_ONLY)); err != nil {
		t.Fatalf("post-restart same-body replay must return existing task, got %v", err)
	}
	got, _ := s.GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: "req-r68"})
	if got.GetStatus() == pb.TaskStatus_TASK_STATUS_CREATED {
		t.Fatal("replay must not reset a started task to CREATED (R68)")
	}
	// 异体重放：AlreadyExists
	if _, err := s.CreateScanTask(ctx, mkReq(pb.ScanMode_SCAN_MODE_SAST_ONLY)); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("post-restart different-body replay want AlreadyExists, got %v", err)
	}
}
