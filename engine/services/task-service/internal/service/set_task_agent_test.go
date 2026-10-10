package service

// TP20-T2 切换 Agent 收口（计划 §2.4 用例②③）：SetTaskAgent 新契约（含 RUNNING/
// 终态/非法 id 拒绝消融）+ PauseTask 换笔专行与 agent_to 传递（含纯暂停消融）。

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// lastLogContains — 任务日志中最后一条含 sub 的行（无=nil）。
func lastLogContains(t *testing.T, s *TaskServiceImpl, taskID, sub string) *pb.TaskLogEntry {
	t.Helper()
	resp, err := s.GetTaskLogs(context.Background(), &pb.GetTaskLogsRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("GetTaskLogs: %v", err)
	}
	var hit *pb.TaskLogEntry
	for i := range resp.GetLogs() {
		if strings.Contains(resp.GetLogs()[i].GetMessage(), sub) {
			hit = resp.GetLogs()[i]
		}
	}
	return hit
}

func logCount(t *testing.T, s *TaskServiceImpl, taskID, sub string) int {
	t.Helper()
	resp, err := s.GetTaskLogs(context.Background(), &pb.GetTaskLogsRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("GetTaskLogs: %v", err)
	}
	n := 0
	for _, l := range resp.GetLogs() {
		if strings.Contains(l.GetMessage(), sub) {
			n++
		}
	}
	return n
}

// ---- SetTaskAgent ----

// pausedTask — 白盒置 PAUSED（StartTask 需 project_path，测试无上传面——
// runningTask 同型直转；PauseTask RPC 路径由 AgentToPassthrough 用例覆盖）。
func pausedTask(t *testing.T, s *TaskServiceImpl) string {
	t.Helper()
	createTask(t, s, "req-sw", pb.ScanMode_SCAN_MODE_AI_ONLY)
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.tasks[tid]
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); err != nil {
		t.Fatalf("transition RUNNING: %v", err)
	}
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_PAUSED, "pause-test"); err != nil {
		t.Fatalf("transition PAUSED: %v", err)
	}
	return tid
}

func TestSetTaskAgent_Paused(t *testing.T) {
	s := newSvc(t)
	tid := pausedTask(t, s)
	cur, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"})
	if err != nil {
		t.Fatalf("SetTaskAgent: %v", err)
	}
	if cur.GetAgent() != "maka" || s.tasks[tid].GetAgent() != "maka" {
		t.Fatalf("换笔未生效: %q/%q", cur.GetAgent(), s.tasks[tid].GetAgent())
	}
	// 专行（PAUSED 文案）
	if line := lastLogContains(t, s, tid, "切换 Agent"); line == nil || !strings.Contains(line.GetMessage(), "（任务已暂停，等待接续）") {
		t.Fatalf("PAUSED 专行缺失/文案不符: %+v", line)
	}
}

func TestSetTaskAgent_Queued(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "req-sw-q", pb.ScanMode_SCAN_MODE_AI_ONLY)
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	s.mu.Lock()
	cur := s.tasks[tid]
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); err != nil {
		s.mu.Unlock()
		t.Fatalf("transition RUNNING: %v", err)
	}
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_FAILED, "test-fail"); err != nil {
		s.mu.Unlock()
		t.Fatalf("transition FAILED: %v", err)
	}
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_QUEUED, "test-queue"); err != nil {
		s.mu.Unlock()
		t.Fatalf("queue: %v", err)
	}
	s.mu.Unlock()
	cur, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"})
	if err != nil {
		t.Fatalf("SetTaskAgent(QUEUED): %v", err)
	}
	if cur.GetAgent() != "maka" || s.tasks[tid].GetAgent() != "maka" {
		t.Fatal("QUEUED 换笔未生效")
	}
	if line := lastLogContains(t, s, tid, "切换 Agent"); line == nil || !strings.Contains(line.GetMessage(), "（排队中，启动时生效）") {
		t.Fatalf("QUEUED 专行文案不符: %+v", line)
	}
}

func TestSetTaskAgent_RunningRejected(t *testing.T) {
	s := newSvc(t)
	runningTask(t, s, "req-sw-r")
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"}); err == nil {
		t.Fatal("RUNNING 应拒绝（走 PauseTask?agent=）")
	}
	if got := s.tasks[tid].GetStatus(); got != pb.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("拒绝路径不得迁移状态: %v", got)
	}
	if got := s.tasks[tid].GetAgent(); got == "maka" {
		t.Fatal("拒绝路径不得换笔")
	}
}

func TestSetTaskAgent_TerminalRejected(t *testing.T) {
	s := newSvc(t)
	createTask(t, s, "req-sw-t", pb.ScanMode_SCAN_MODE_AI_ONLY)
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	s.mu.Lock()
	if err := s.transitionLocked(s.tasks[tid], pb.TaskStatus_TASK_STATUS_CANCELLED, "test-cancel"); err != nil {
		s.mu.Unlock()
		t.Fatalf("cancel: %v", err)
	}
	s.mu.Unlock()
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"}); err == nil {
		t.Fatal("终态应拒绝")
	}
}

func TestSetTaskAgent_InvalidAndIdempotent(t *testing.T) {
	s := newSvc(t)
	tid := pausedTask(t, s)
	// 非法 id：先校验后动作（状态/快照/日志三不动）
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "Bad_Id"}); err == nil {
		t.Fatal("非法 agent 应拒绝")
	}
	if got := s.tasks[tid].GetAgent(); got == "Bad_Id" {
		t.Fatalf("非法值污染快照: %q", got)
	}
	if line := lastLogContains(t, s, tid, "切换 Agent"); line != nil {
		t.Fatalf("非法值不得写专行: %+v", line)
	}
	// 同 agent 重设=幂等零动作零专行
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"}); err != nil {
		t.Fatalf("首次切换: %v", err)
	}
	n1 := logCount(t, s, tid, "切换 Agent")
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid, Agent: "maka"}); err != nil {
		t.Fatalf("幂等重设: %v", err)
	}
	if n2 := logCount(t, s, tid, "切换 Agent"); n2 != n1 {
		t.Fatalf("同 agent 重设应零专行: %d → %d", n1, n2)
	}
	// 空 agent 拒绝
	if _, err := s.SetTaskAgent(context.Background(), &pb.SetTaskAgentRequest{TaskId: tid}); err == nil {
		t.Fatal("空 agent 应拒绝")
	}
}

// ---- PauseTask 专行 + agent_to 传递（用例③）----

// fakeRuntime — 捕获 PauseAnalysis 请求（agent_to 传递断言的观测面）。
type fakeRuntime struct {
	pb.UnimplementedRuntimeServiceServer
	mu   sync.Mutex
	reqs []*pb.PauseAnalysisRequest
}

func (f *fakeRuntime) PauseAnalysis(_ context.Context, req *pb.PauseAnalysisRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return &emptypb.Empty{}, nil
}

func TestPauseTask_AgentToPassthroughAndLogLine(t *testing.T) {
	// 真 gRPC 捕获面：dshControl 拨 CODEAUDIT_RUNTIME_ADDR（尽力而为通道——
	// 假服务在场即真捕获，消融侧见 TestPauseTask_PurePauseNoAgent）
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无本地监听能力: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeRuntime{}
	pb.RegisterRuntimeServiceServer(srv, fake)
	go srv.Serve(lis)
	defer srv.Stop()

	// R138/P3-7：dshControl 地址构造时定格——须先指假服务再 newSvc（原事后改 env
	// 依赖调用时读取的旧形态）
	t.Setenv("CODEAUDIT_RUNTIME_ADDR", lis.Addr().String())
	s := newSvc(t)
	runningTask(t, s, "req-pt")
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	cur, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid, Agent: "maka"})
	if err != nil {
		t.Fatalf("pause+handoff: %v", err)
	}
	if cur.GetAgent() != "maka" {
		t.Fatal("换笔未生效")
	}
	// 专行：from 空=「缺省」标注
	if line := lastLogContains(t, s, tid, "切换 Agent"); line == nil ||
		!strings.Contains(line.GetMessage(), "切换 Agent：缺省 → maka（任务已暂停，等待接续）") {
		t.Fatalf("专行文案不符: %+v", line)
	}
	// agent_to 传递：PauseAnalysis 请求携带目标
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.reqs) == 0 {
		t.Fatal("PauseAnalysis 未被调用（dshControl 尽力而为通道）")
	}
	last := fake.reqs[len(fake.reqs)-1]
	if last.GetAgentTo() != "maka" {
		t.Fatalf("agent_to 未传递: %+v", last)
	}
}

func TestPauseTask_PurePauseNoAgent(t *testing.T) {
	s := newSvc(t)
	runningTask(t, s, "req-pp")
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	if _, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid}); err != nil {
		t.Fatalf("纯暂停: %v", err)
	}
	if line := lastLogContains(t, s, tid, "切换 Agent"); line != nil {
		t.Fatalf("纯暂停零专行（消融）: %+v", line)
	}
	if got := s.tasks[tid].GetAgent(); got != "" {
		t.Fatalf("纯暂停不得换笔: %q", got)
	}
}
