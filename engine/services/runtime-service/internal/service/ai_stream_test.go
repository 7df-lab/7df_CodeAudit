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
	pb.RuntimeService_StreamAIInteractionLogServer
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

func newStreamSvc() *RuntimeServiceImpl {
	return &RuntimeServiceImpl{aiLogs: newAILogStore("")} // 不落盘，纯内存
}

func TestStreamAIInteractionLog_PushOnWriteAndFinish(t *testing.T) {
	s := newStreamSvc()
	entry := s.aiLogs.writer("st-1", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)

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
	s.aiLogs.writer("st-2", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN).write([]byte("迟到的条目"))

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

// TP20 缺损批 A4：终态大日志排干纪律——complete=true 且内容超单帧
// 上限时，流必须背靠背排干全部字节，且只有末帧携带 complete=true（原两条件版
// 首帧即带 complete 关流=帧语义自相矛盾+网关重臂 2s/帧低效排干）。
func TestStreamAIInteractionLog_TerminalLargeLogDrains(t *testing.T) {
	s := newStreamSvc()
	e := s.aiLogs.writer("st-drain", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	big := make([]byte, 3500) // > 3×maxBytes(1024)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	e.write(big)
	e.finish() // 终态：complete=true, total=3500

	fs := newFakeAIStream()
	done := make(chan error, 1)
	go func() {
		done <- s.StreamAIInteractionLog(&pb.StreamAIInteractionLogRequest{
			TaskId: "st-drain", Cursor: 0, MaxBytes: 1024,
		}, fs)
	}()
	var frames []*pb.GetAIInteractionLogResponse
	for {
		select {
		case r := <-fs.frames:
			frames = append(frames, r)
		case err := <-done:
			if err != nil {
				t.Fatalf("stream err: %v", err)
			}
			for { // done 与已缓冲帧同就绪时 select 随机——结束后补排空缓冲
				select {
				case r := <-fs.frames:
					frames = append(frames, r)
				default:
					goto drained
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("drain not finishing")
		}
	}
drained:
	if len(frames) != 4 { // 1024+1024+1024+428
		t.Fatalf("应有 4 帧背靠背排干（1024×3+428），实得 %d 帧", len(frames))
	}
	gotBytes := 0
	for i, f := range frames {
		gotBytes += len(f.GetChunk())
		if i < len(frames)-1 && f.GetComplete() {
			t.Fatalf("帧 %d/%d 未排干即带 complete=true（next=%d total=%d）——帧语义失真",
				i+1, len(frames), f.GetNextCursor(), f.GetTotalBytes())
		}
	}
	if !frames[len(frames)-1].GetComplete() {
		t.Fatal("末帧应携带 complete=true")
	}
	if gotBytes != 3500 || frames[len(frames)-1].GetNextCursor() != 3500 {
		t.Fatalf("排干不完整: got=%dB next=%d", gotBytes, frames[len(frames)-1].GetNextCursor())
	}
}

// F1：运行中流（complete=false）主面积压超单帧上限时，
// 订阅必须背靠背静默排干——原 `complete && next < total` 前缀使非终态积压在首帧
// 后 park 等新写（消融实证 2500B/1024 窗仅送一帧）。
func TestStreamAIInteractionLog_RunningBacklogDrains(t *testing.T) {
	s := newStreamSvc()
	e := s.aiLogs.writer("st-runbacklog", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	big := make([]byte, 2500)
	for i := range big {
		big[i] = byte('A' + i%26)
	}
	e.write(big) // 运行中积压（不 finish——complete=false）

	fs := newFakeAIStream()
	done := make(chan error, 1)
	go func() {
		done <- s.StreamAIInteractionLog(&pb.StreamAIInteractionLogRequest{
			TaskId: "st-runbacklog", Cursor: 0, MaxBytes: 1024,
		}, fs)
	}()
	var frames []*pb.GetAIInteractionLogResponse
	deadline := time.After(800 * time.Millisecond) // 静默窗：无新写也应排干
collect:
	for {
		select {
		case r := <-fs.frames:
			frames = append(frames, r)
		case <-deadline:
			break collect
		case err := <-done:
			t.Fatalf("运行中流不应自行收束: %v", err)
		}
	}
	fs.cancel()
	got := 0
	for _, f := range frames {
		got += len(f.GetChunk())
	}
	if got != 2500 {
		t.Fatalf("运行中积压应静默排干 2500B, 实得 %dB / %d 帧——积压 park 等新写（F1 回归）", got, len(frames))
	}
}
