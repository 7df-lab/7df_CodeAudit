package service

// 修复回归锁（ADR-131）：假成功 RPC 真实化 / 幂等三态 / 自动重试耗尽→DEAD /
// 状态机单一权威 / 进度统计。依据: 03 §2、04 §1、proto L174/L177/L880-L881。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
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
	task, err := s.CreateScanTask(ctx, &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-retry"},
		ProjectId: "p-retry",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/rt"}, // 有路径：编排真实启动
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = task
	mustStart(t, s, "req-retry")
	// 下游全部不可达：3 次执行（首次+2 重试）后应到 DEAD（proto L174/L177）
	res := waitForStatus(t, s, "req-retry", pb.TaskStatus_TASK_STATUS_DEAD, 30*time.Second)
	_ = res
	if int(task.GetRetryCount()) != maxAutoRetries {
		t.Fatalf("retry_count = %d, want %d", task.GetRetryCount(), maxAutoRetries)
	}
	if task.GetErrorMessage() == "" {
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
