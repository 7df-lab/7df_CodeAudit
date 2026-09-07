package handler

// ADR-213 回归：流式 watch 路此前零测试覆盖（既有 WS 用例的假体未实现
// StreamTaskSnapshot，全部落入轮询路）——三缺陷因此长期潜伏：
//   ① 回退/收束后上游流不撤销，泵 goroutine 泄漏；
//   ② 回退前已吸收未冲刷的增量丢失（游标已越过，轮询取不到）；
//   ③ 无 DSH 连接时 AI 流立即关闭 → `aiEnded && !sawAIComplete` 恒真 →
//      流式路对 SAST-only 部署是死路径（恒回退轮询）。
// 本文件用真实流式假体钉住修复。

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeStreamTask — 实现流式订阅的最小 TaskService 假体。
type fakeStreamTask struct {
	pb.UnimplementedTaskServiceServer

	streamDeltas []*pb.TaskSnapshotDelta // 开场下发的增量序列（下发完即断流）
	breakStream  bool                    // 下发完后返回错误（模拟断流）
	holdOpen     bool                    // 下发完后保持流开着（不回退）

	streamCtxDone chan struct{} // 服务端观测到流 ctx 被撤销（①的判据）

	pollGetTask   int          // GetScanTask 被调次数（判据：回退与否）
	scanStatus    pb.TaskStatus
	getScanErr    error  // 非 nil 时按序返回该错误（③瞬断判据）
	transientFails int   // >0 时前 N 次 GetScanTask 返回 Unavailable 后自愈
}

func (f *fakeStreamTask) StreamTaskSnapshot(req *pb.StreamTaskSnapshotRequest, srv pb.TaskService_StreamTaskSnapshotServer) error {
	ctx := srv.Context()
	if f.streamCtxDone != nil {
		go func() {
			<-ctx.Done()
			close(f.streamCtxDone)
		}()
	}
	for _, d := range f.streamDeltas {
		if err := srv.Send(d); err != nil {
			return err
		}
	}
	if f.breakStream {
		return status.Error(codes.Internal, "synthetic stream break")
	}
	if f.holdOpen {
		<-ctx.Done()
	}
	return nil
}

func (f *fakeStreamTask) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	f.pollGetTask++
	if f.transientFails > 0 {
		f.transientFails--
		return nil, statusErrorUnavailable()
	}
	if f.getScanErr != nil {
		return nil, f.getScanErr
	}
	return &pb.ScanTask{TaskId: req.GetTaskId(), Status: f.scanStatus}, nil
}

func (f *fakeStreamTask) GetTaskProgress(ctx context.Context, req *pb.GetTaskProgressRequest) (*pb.TaskProgress, error) {
	return &pb.TaskProgress{TaskId: req.GetTaskId()}, nil
}

func (f *fakeStreamTask) GetTaskLogs(ctx context.Context, req *pb.GetTaskLogsRequest) (*pb.GetTaskLogsResponse, error) {
	return &pb.GetTaskLogsResponse{Logs: []*pb.TaskLogEntry{}}, nil // 轮询路恒空：日志只可能来自流式增量
}

func statusErrorUnavailable() error {
	return status.Errorf(codes.Unavailable, "synthetic transient outage")
}

// fakeDSHStream — 最小 DSHRuntimeService 假体：AI 流可配置延迟/增量/断流，
// 用于钉住"AI 侧断流触发回退"路径上的冲刷与上游流撤销。
type fakeDSHStream struct {
	pb.UnimplementedDSHRuntimeServiceServer
	aiDelayMs int  // 订阅后延迟（错开任务流事件，制造确定性时序）
	aiChunks  int  // 先下发的增量帧数
	aiBreak   bool // 下发完即断流（不 complete）
}

func (f *fakeDSHStream) StreamAIInteractionLog(req *pb.StreamAIInteractionLogRequest, srv pb.DSHRuntimeService_StreamAIInteractionLogServer) error {
	if f.aiDelayMs > 0 {
		time.Sleep(time.Duration(f.aiDelayMs) * time.Millisecond)
	}
	for i := 0; i < f.aiChunks; i++ {
		if err := srv.Send(&pb.GetAIInteractionLogResponse{Chunk: []byte("ai-chunk"), NextCursor: int64(i + 1)}); err != nil {
			return err
		}
	}
	if f.aiBreak {
		return status.Error(codes.Internal, "synthetic ai stream break")
	}
	<-srv.Context().Done()
	return nil
}

func (f *fakeDSHStream) GetAIInteractionLog(ctx context.Context, req *pb.GetAIInteractionLogRequest) (*pb.GetAIInteractionLogResponse, error) {
	return &pb.GetAIInteractionLogResponse{Complete: true}, nil // 轮询路无增量：增量只能来自流式冲刷
}

// startStreamBackends — 单 TaskService 假体（无 DSH：dshConn==nil 场景）。
func startStreamBackends(t *testing.T, fake *fakeStreamTask) *httptest.Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterTaskServiceServer(s, fake)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	tr := NewTranscoder(BackendAddrs{
		TaskAddr:    lis.Addr().String(),
		CallTimeoutS: 5,
	})
	srv := httptest.NewServer(tr.Handler())
	t.Cleanup(func() {
		tr.Close()
		srv.Close()
	})
	return srv
}

// startStreamBackendsBoth — Task + DSH 双假体（AI 侧断流触发回退的场景）。
func startStreamBackendsBoth(t *testing.T, fake *fakeStreamTask, dshFake *fakeDSHStream) *httptest.Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterTaskServiceServer(s, fake)
	pb.RegisterDSHRuntimeServiceServer(s, dshFake)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	tr := NewTranscoder(BackendAddrs{
		TaskAddr:      lis.Addr().String(),
		DSHRuntimeAddr: lis.Addr().String(),
		CallTimeoutS:  5,
	})
	srv := httptest.NewServer(tr.Handler())
	t.Cleanup(func() {
		tr.Close()
		srv.Close()
	})
	return srv
}

func dialWatch(t *testing.T, srv *httptest.Server, taskID string) *wsClient {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/tasks/"+taskID+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return &wsClient{conn: c}
}

type wsClient struct{ conn *websocket.Conn }

// readFrames — 读到 deadline，把全部帧按原样返回。
func (w *wsClient) readFrames(t *testing.T) []map[string]interface{} {
	t.Helper()
	var frames []map[string]interface{}
	for {
		_, raw, err := w.conn.ReadMessage()
		if err != nil {
			return frames
		}
		var f map[string]interface{}
		if err := json.Unmarshal(raw, &f); err == nil {
			frames = append(frames, f)
		}
	}
}

// ADR-213② 回归：断流前已吸收未冲刷的日志增量必须在回退前推给客户端——
// 游标已越过该内容，轮询路取不到，不冲即丢（丢的恰是断流前最后一窗）。
// 时序设计（确定性）：任务流 t0 下发增量后断流（close 分支 continue 不触发回退判定），
// AI 流 t0+30ms 下发增量后断流——该迭代直达 taskEnded 判定（早于 50ms 合并窗定时器），
// 迫使"回退前冲刷"块成为唯一投递路径（定时器路径锁不住本修复，变异自检实证）。
func TestStreamWatch_FallbackFlushesPendingLogs(t *testing.T) {
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Logs: &pb.GetTaskLogsResponse{Logs: []*pb.TaskLogEntry{
			{LogId: "l-1", Message: "tail-line-before-break"},
		}}}},
		breakStream: true,
		scanStatus:  pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	dshFake := &fakeDSHStream{aiDelayMs: 30, aiChunks: 1, aiBreak: true}
	srv := startStreamBackendsBoth(t, fake, dshFake)

	frames := dialWatch(t, srv, "t-flush").readFrames(t)
	foundLog, foundAI := false, false
	for _, f := range frames {
		if logs, ok := f["logs"].(map[string]interface{}); ok {
			if ls, ok := logs["logs"].([]interface{}); ok {
				for _, l := range ls {
					if entry, ok := l.(map[string]interface{}); ok && entry["message"] == "tail-line-before-break" {
						foundLog = true
					}
				}
			}
		}
		if ai, ok := f["ai"].(map[string]interface{}); ok && ai["chunk"] != "" {
			foundAI = true
		}
	}
	if !foundLog {
		t.Fatalf("absorbed log line lost on fallback (got %d frames)", len(frames))
	}
	if !foundAI {
		t.Fatalf("absorbed ai chunk lost on fallback (got %d frames)", len(frames))
	}
}

// ADR-213① 回归：streamWatch 退出（此处=回退轮询）必须撤销上游流——
// 否则泵 Recv 挂死在已弃置的流上，goroutine+上游流逐次泄漏。
// 时序设计（确定性）：任务流 holdOpen（服务端只等 ctx.Done——只有客户端侧
// cancelStream 能撤销它）；AI 流立即断流触发回退。原测试用 breakStream，
// 服务端 handler return 也会取消流 ctx，判据恒真（变异自检实证假锁），
// 必须用 holdOpen 才能区分"回退时撤销"与"handler 自然退出"。
func TestStreamWatch_FallbackCancelsUpstreamStream(t *testing.T) {
	fake := &fakeStreamTask{
		streamDeltas:  []*pb.TaskSnapshotDelta{{}},
		holdOpen:      true,
		scanStatus:    pb.TaskStatus_TASK_STATUS_RUNNING,
		streamCtxDone: make(chan struct{}),
	}
	dshFake := &fakeDSHStream{aiBreak: true} // AI 流立即断流（未 complete）→ 触发回退
	srv := startStreamBackendsBoth(t, fake, dshFake)
	w := dialWatch(t, srv, "t-cancel")
	_ = w.readFrames(t) // 走完回退

	select {
	case <-fake.streamCtxDone:
		// 上游流已被撤销：泵不会挂死在弃置的流上
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream stream ctx not cancelled after fallback (pump leak)")
	}
}

// ADR-213③补 回归：无 DSH 连接时流式路不得恒回退——AI 流本就不存在，
// `aiEnded && !sawAIComplete` 不应触发（原实现使 SAST-only 部署的流式路成死路径）。
func TestStreamWatch_NoDSH_StreamsStayOnStreamPath(t *testing.T) {
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Logs: &pb.GetTaskLogsResponse{Logs: []*pb.TaskLogEntry{
			{LogId: "l-1", Message: "streamed-line"},
		}}}},
		holdOpen:  true, // 流保持打开：任务 RUNNING 不到终态
		scanStatus: pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	srv := startStreamBackends(t, fake)
	frames := dialWatch(t, srv, "t-stay").readFrames(t)
	if fake.pollGetTask != 0 {
		t.Fatalf("fallback to poll happened (GetScanTask called %d times)", fake.pollGetTask)
	}
	found := false
	for _, f := range frames {
		if logs, ok := f["logs"].(map[string]interface{}); ok {
			if ls, ok := logs["logs"].([]interface{}); ok && len(ls) > 0 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("streamed log line never delivered")
	}
}

// ADR-213③ 回归：轮询路瞬时错误（Unavailable）容忍连续数拍——后端一次抖动
// 不得拆线（原实现任何错误即 1011，全员断线+重连风暴）。
func TestPollWatch_TransientErrorTolerated(t *testing.T) {
	fake := &fakeStreamTask{ // 流式开局即断 → 回退轮询路
		scanStatus:     pb.TaskStatus_TASK_STATUS_RUNNING,
		transientFails: 3,
	}
	srv := startStreamBackends(t, fake)
	w := dialWatch(t, srv, "t-transient")
	frames := w.readFrames(t) // 读到关闭为止

	if fake.pollGetTask < 4 {
		t.Fatalf("expected transient failures then recovery, GetScanTask called %d times", fake.pollGetTask)
	}
	sawRunning := false
	for _, f := range frames {
		if task, ok := f["task"].(map[string]interface{}); ok && task["status"] == "TASK_STATUS_RUNNING" {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatalf("watch died on transient errors (no RUNNING frame after recovery)")
	}
}
