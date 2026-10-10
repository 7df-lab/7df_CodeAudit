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
	"sync/atomic"

	"github.com/codeaudit/services/gateway-service/internal/middleware"
	"net"
	"net/http"
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

	pollGetTask    atomic.Int32 // GetScanTask 被调次数（判据：回退与否；跨 gRPC 协程原子）
	scanStatus     pb.TaskStatus
	getScanErr     error // 非 nil 时按序返回该错误（③瞬断判据）
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
	f.pollGetTask.Add(1)
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

// fakeDSHStream — 最小 RuntimeService 假体：AI 流可配置延迟/增量/断流，
// 用于钉住"AI 侧断流触发回退"路径上的冲刷与上游流撤销。
type fakeDSHStream struct {
	pb.UnimplementedRuntimeServiceServer
	aiDelayMs int  // 订阅后延迟（错开任务流事件，制造确定性时序）
	aiChunks  int  // 先下发的增量帧数
	aiBreak   bool // 下发完即断流（不 complete）

	// R132 两纪元档：订阅 #1 发纪元1 增量+complete 终帧后关流（Run 收束语义），
	// 订阅 #2（网关重臂）发纪元2 增量+complete 后关流。任务面恒非终态——
	// 钉住"收束关流而任务未终态=纪元边界，必须重订阅续接"。
	twoEpochs bool
	subs      atomic.Int32 // 订阅次数（StreamAIInteractionLog 调用计数；跨 gRPC 协程原子）
}

func (f *fakeDSHStream) StreamAIInteractionLog(req *pb.StreamAIInteractionLogRequest, srv pb.RuntimeService_StreamAIInteractionLogServer) error {
	if f.twoEpochs {
		if f.subs.Add(1) == 1 {
			return srv.Send(&pb.GetAIInteractionLogResponse{
				Chunk: []byte("epoch1"), NextCursor: 7, TotalBytes: 7, Complete: true,
			}) // 发完即 return nil：服务端关流（纪元1 收束）
		}
		return srv.Send(&pb.GetAIInteractionLogResponse{
			Chunk: []byte("epoch2"), NextCursor: 14, TotalBytes: 14, Complete: true,
		})
	}
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
		TaskAddr:     lis.Addr().String(),
		CallTimeoutS: 5,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.UserRoleKey, "ROLE_ADMIN") // R89: 机制测试按 admin 走门禁
		tr.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(func() {
		tr.Close()
		srv.Close()
	})
	return srv
}

// startStreamBackendsBoth — Task + DSH 双假体（AI 侧断流触发回退的场景）。
func startStreamBackendsBoth(t *testing.T, fake *fakeStreamTask, dshFake pb.RuntimeServiceServer) *httptest.Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterTaskServiceServer(s, fake)
	pb.RegisterRuntimeServiceServer(s, dshFake)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	tr := NewTranscoder(BackendAddrs{
		TaskAddr:     lis.Addr().String(),
		RuntimeAddr:  lis.Addr().String(),
		CallTimeoutS: 5,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.UserRoleKey, "ROLE_ADMIN") // R89: 机制测试按 admin 走门禁
		tr.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
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
		holdOpen:   true, // 流保持打开：任务 RUNNING 不到终态
		scanStatus: pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	srv := startStreamBackends(t, fake)
	frames := dialWatch(t, srv, "t-stay").readFrames(t)
	if fake.pollGetTask.Load() != 0 {
		t.Fatalf("fallback to poll happened (GetScanTask called %d times)", fake.pollGetTask.Load())
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

	if fake.pollGetTask.Load() < 4 {
		t.Fatalf("expected transient failures then recovery, GetScanTask called %d times", fake.pollGetTask.Load())
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

// R132（TP20-T3）回归：多纪元续流——AI 流收束（complete 终帧，Run 收束语义）
// 而任务未终态时，网关必须重臂 AI 订阅续接纪元2 字节。旧实现 aiCh 关闭即
// nil 永久阻塞且永不重订阅=纪元 N+1 字节永不转发（ui_check 切换全路径实证：
// 面板冻结在纪元1 末尾、后端持续增长、WS 帧只余任务面）。
func TestStreamWatch_MultiEpochRearm(t *testing.T) {
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Task: &pb.ScanTask{
			TaskId: "t-rearm", Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		}}},
		holdOpen:   true, // 任务恒非终态：纪元边界后必须留在流式路续接
		scanStatus: pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	dshFake := &fakeDSHStream{twoEpochs: true}
	srv := startStreamBackendsBoth(t, fake, dshFake)
	frames := dialWatch(t, srv, "t-rearm").readFrames(t) // 5s 读窗 > aiRearmDelay(2s)+冲刷窗

	sawEpoch1, sawEpoch2 := false, false
	for _, f := range frames {
		ai, ok := f["ai"].(map[string]interface{})
		if !ok {
			continue
		}
		// proto3 JSON 口径：int64 编码为字符串（web 客户端 Number() 同源处理）；
		// chunk=base64（"epoch1"=ZXBvY2gx / "epoch2"=ZXBvY2gy）。精确等值断言：
		// 旧 push 整对象保留会把已送纪元字节重复 append 进下一帧（chunk 变复合串）。
		switch ai["next_cursor"] {
		case "7":
			if ai["chunk"] == "ZXBvY2gx" {
				sawEpoch1 = true
			}
		case "14":
			if ai["chunk"] == "ZXBvY2gy" {
				sawEpoch2 = true
			}
		}
	}
	if !sawEpoch1 || !sawEpoch2 {
		t.Fatalf("两纪元字节必须全达（epoch1=%v epoch2=%v, %d frames, subs=%d）",
			sawEpoch1, sawEpoch2, len(frames), dshFake.subs.Load())
	}
	if dshFake.subs.Load() < 2 {
		t.Fatalf("重臂未发生（订阅次数=%d）——非终态收束被当终点", dshFake.subs.Load())
	}
	if fake.pollGetTask.Load() != 0 {
		t.Fatalf("续流不应回退轮询（GetScanTask=%d）", fake.pollGetTask.Load())
	}
}

// TP20 缺损批 A3：静默期保活数据帧——TURN 方言整回合静默/长挂起
// 期，客户端 45s 看门狗靠 ping（控制帧 JS 不可见）喂不活，误杀重连循环。网关
// 以 wsIdleFrameEvery 节拍补发当前快照帧。测试缩拍至 150ms 验证（>keepalive 拍
// 20s 不可等）——keepalive ticker 同拍驱动，故缩拍需同步两个变量。
func TestStreamWatch_IdleKeepaliveFrame(t *testing.T) {
	origIdle := wsIdleFrameEvery
	wsIdleFrameEvery = 150 * time.Millisecond
	t.Cleanup(func() { wsIdleFrameEvery = origIdle })
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Task: &pb.ScanTask{
			TaskId: "t-idle", Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		}}},
		holdOpen:   true, // 全静默：无任何后续增量
		scanStatus: pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	dshFake := &fakeDSHStream{aiChunks: 1} // AI 流开局一帧后持流静默
	srv := startStreamBackendsBoth(t, fake, dshFake)
	c := dialWatch(t, srv, "t-idle")
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frames := c.readFrames(t)
	if len(frames) < 3 {
		t.Fatalf("静默期应有保活帧持续到达（≥3 帧/2s），实得 %d——看门狗将误杀", len(frames))
	}
	if fake.pollGetTask.Load() != 0 {
		t.Fatalf("保活帧不应触发轮询回退: polls=%d", fake.pollGetTask.Load())
	}
}

// TP20 缺损批 A5：终态收束排干带——sawAIComplete 跨纪元残留时
// （纪元1 真收束、终态到达而末纪元游标未排干），不得以 "task settled" 关流截断
// 尾部；应并入宽限/回退轮询排干。假体：AI 流发 complete 终帧但 total(100) 超
// next(7)=协议允许的未排干终帧形态（A4 修复后 runtime 不再产生，网关独立防御）。
type fakeUndrainedSettle struct {
	pb.UnimplementedRuntimeServiceServer
}

func (f *fakeUndrainedSettle) StreamAIInteractionLog(req *pb.StreamAIInteractionLogRequest, srv pb.RuntimeService_StreamAIInteractionLogServer) error {
	return srv.Send(&pb.GetAIInteractionLogResponse{
		Chunk: []byte("7bytes"), NextCursor: 7, TotalBytes: 100, Complete: true,
	})
}

func (f *fakeUndrainedSettle) GetAIInteractionLog(ctx context.Context, req *pb.GetAIInteractionLogRequest) (*pb.GetAIInteractionLogResponse, error) {
	return &pb.GetAIInteractionLogResponse{Complete: true, TotalBytes: 100}, nil // 轮询路同形态（排干校验同拦）
}

func TestStreamWatch_TerminalUndrainedDoesNotSettle(t *testing.T) {
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Task: &pb.ScanTask{
			TaskId: "t-undrained", Status: pb.TaskStatus_TASK_STATUS_COMPLETED,
		}}},
		breakStream: true, // 任务面即终态并断流（taskEnded 立即成立）
		scanStatus:  pb.TaskStatus_TASK_STATUS_COMPLETED,
	}
	dshFake := &fakeUndrainedSettle{}
	srv := startStreamBackendsBoth(t, fake, dshFake)
	c := dialWatch(t, srv, "t-undrained")
	_ = c.conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	_ = c.readFrames(t) // 读窗须过宽限（aiExpectGrace 3s+1s）——到点后未排干应回退轮询
	// 判据：宽限到点后已转入轮询兜底排干（pollGetTask>0）——旧实现立即
	// writeClose("task settled") 且零轮询（末纪元尾部截断）。
	if fake.pollGetTask.Load() == 0 {
		t.Fatalf("未排干终态被提前收束（截断尾部）——应回退轮询排干")
	}
}

// F2：静默保活帧不得携带 pendAI 保留旗面——上一次真帧的
// complete=true 不得在静默期重复出现（客户端「已收束」/归档折叠抖动，TURN 静默
// 帧日志实证 complete=True 重复帧）。
type fakeSettledThenQuiet struct {
	pb.UnimplementedRuntimeServiceServer
}

func (f *fakeSettledThenQuiet) StreamAIInteractionLog(req *pb.StreamAIInteractionLogRequest, srv pb.RuntimeService_StreamAIInteractionLogServer) error {
	if err := srv.Send(&pb.GetAIInteractionLogResponse{
		Chunk: []byte("7bytes"), NextCursor: 7, TotalBytes: 7, Complete: true,
	}); err != nil {
		return err
	}
	<-srv.Context().Done() // 真帧后持流静默
	return nil
}

func (f *fakeSettledThenQuiet) GetAIInteractionLog(ctx context.Context, req *pb.GetAIInteractionLogRequest) (*pb.GetAIInteractionLogResponse, error) {
	return &pb.GetAIInteractionLogResponse{Complete: true, TotalBytes: 7}, nil
}

func TestStreamWatch_IdleFrameOmitsStaleFlags(t *testing.T) {
	origIdle := wsIdleFrameEvery
	wsIdleFrameEvery = 120 * time.Millisecond
	t.Cleanup(func() { wsIdleFrameEvery = origIdle })
	fake := &fakeStreamTask{
		streamDeltas: []*pb.TaskSnapshotDelta{{Task: &pb.ScanTask{
			TaskId: "t-idleflag", Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		}}},
		holdOpen:   true,
		scanStatus: pb.TaskStatus_TASK_STATUS_RUNNING,
	}
	srv := startStreamBackendsBoth(t, fake, &fakeSettledThenQuiet{})
	c := dialWatch(t, srv, "t-idleflag")
	_ = c.conn.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
	frames := c.readFrames(t)
	completeTrue := 0
	for _, f := range frames {
		if ai, ok := f["ai"].(map[string]interface{}); ok && ai["complete"] == true {
			completeTrue++
		}
	}
	if completeTrue != 1 {
		t.Fatalf("complete=true 帧应恰 1 帧（首真帧），静默保活帧不得携带过期旗面——实得 %d/%d 帧", completeTrue, len(frames))
	}
	if len(frames) < 4 {
		t.Fatalf("静默期保活帧应持续到达（≥4 帧/1.2s），实得 %d", len(frames))
	}
}
