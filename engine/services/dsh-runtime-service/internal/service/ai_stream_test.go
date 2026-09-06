// AI 交互日志订阅流回归（ADR-189）：写入即时唤醒推增量/终帧 complete 后关流/
// 字节游标增量语义与 GetAIInteractionLog 一致/条目未创建时等待出现。
package service

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

// fakeAIStream — StreamAIInteractionLogServer 假实现（捕获推帧）。
type fakeAIStream struct {
	pb.DSHRuntimeService_StreamAIInteractionLogServer
	frames chan *pb.GetAIInteractionLogResponse
	ctx    context.Context
	cancel context.CancelFunc
}

func newFakeAIStream() *fakeAIStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeAIStream{frames: make(chan *pb.GetAIInteractionLogResponse, 64), ctx: ctx, cancel: cancel}
}

func (f *fakeAIStream) Send(r *pb.GetAIInteractionLogResponse) error {
	f.frames <- r
	return nil
}
func (f *fakeAIStream) Context() context.Context { return f.ctx }

func waitAIFrame(t *testing.T, f *fakeAIStream, timeout time.Duration) *pb.GetAIInteractionLogResponse {
	t.Helper()
	select {
	case r := <-f.frames:
		return r
	case <-time.After(timeout):
		t.Fatal("ai stream frame not delivered in time")
		return nil
	}
}

func newStreamSvc() *DSHRuntimeServiceImpl {
	return &DSHRuntimeServiceImpl{aiLogs: newAILogStore("")} // 不落盘，纯内存
}

func TestStreamAIInteractionLog_PushOnWriteAndFinish(t *testing.T) {
	s := newStreamSvc()
	entry := s.aiLogs.writer("st-1")

	fs := newFakeAIStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.StreamAIInteractionLog(&pb.StreamAIInteractionLogRequest{TaskId: "st-1"}, fs)
	}()

	// 订阅后写入：即时推送增量（不等到终态）
	start := time.Now()
	entry.write([]byte("第一段交互"))
	first := waitAIFrame(t, fs, 2*time.Second)
	if wake := time.Since(start); wake > time.Second {
		t.Fatalf("write wake too slow: %v", wake)
	}
	if string(first.GetChunk()) != "第一段交互" || first.GetComplete() {
		t.Fatalf("first frame: chunk=%q complete=%v", first.GetChunk(), first.GetComplete())
	}

	// 连续多次写：信号合并，但唤醒后一次读全增量（游标单调前进）
	entry.write([]byte("第二段"))
	entry.write([]byte("第三段"))
	second := waitAIFrame(t, fs, 2*time.Second)
	if string(second.GetChunk()) != "第二段第三段" {
		t.Fatalf("coalesced increment: %q", second.GetChunk())
	}

	// finish：complete 终帧后流收束
	entry.finish()
	last := waitAIFrame(t, fs, 2*time.Second)
	if !last.GetComplete() || len(last.GetChunk()) != 0 {
		t.Fatalf("final frame: complete=%v chunk=%d bytes", last.GetComplete(), len(last.GetChunk()))
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stream must close cleanly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not return after complete frame")
	}
}

func TestStreamAIInteractionLog_WaitsForEntry(t *testing.T) {
	s := newStreamSvc()
	fs := newFakeAIStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.StreamAIInteractionLog(&pb.StreamAIInteractionLogRequest{TaskId: "st-2"}, fs)
	}()

	// 300ms 后条目才出现（GUI 在 AI 阶段开始前订阅的场景）
	time.Sleep(300 * time.Millisecond)
	s.aiLogs.writer("st-2").write([]byte("迟到的条目"))

	got := waitAIFrame(t, fs, 3*time.Second)
	if string(got.GetChunk()) != "迟到的条目" {
		t.Fatalf("late entry chunk: %q", got.GetChunk())
	}
	fs.cancel()
}

func TestStreamAIInteractionLog_EmptyTaskID(t *testing.T) {
	s := newStreamSvc()
	fs := newFakeAIStream()
	if err := s.StreamAIInteractionLog(&pb.StreamAIInteractionLogRequest{}, fs); err == nil {
		t.Fatal("empty task_id must error")
	}
}
