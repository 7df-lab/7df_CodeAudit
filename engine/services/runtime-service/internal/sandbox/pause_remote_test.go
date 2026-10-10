package sandbox

// PauseGate 上收双通道测试（HA P1-b，ADR-250）：本地 gate（快路径）OR 任务状态
// 机 PAUSED（权威路径）——挂起/解除/冻结回调恰一次四态。
// 依据: 主仓库 docs/designs/platform-ha-2026-09-19.md §1 Q9 + ADR-250。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func stubTaskState(states ...func() string) (restore func()) {
	old := TaskStateFunc
	var i int32
	TaskStateFunc = func(ctx context.Context, taskID string) (string, error) {
		f := states[atomic.AddInt32(&i, 1)-1]
		if f == nil {
			f = states[len(states)-1]
		}
		return f(), nil
	}
	return func() { TaskStateFunc = old }
}

func waitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// 远端 PAUSED→挂起（冻结回调恰一次）→远端解除→放行。
func TestWaitFrozen_RemotePauseRemoteResume(t *testing.T) {
	restore := stubTaskState(func() string { return "TASK_STATUS_PAUSED" }, func() string { return "TASK_STATUS_RUNNING" })
	defer restore()
	OnFrozen("t-rr", func() {})
	ClearFrozen("t-rr")
	freezeCount := int32(0)
	OnFrozen("t-rr", func() { atomic.AddInt32(&freezeCount, 1) })
	defer ClearFrozen("t-rr")
	g := GateFor("t-rr")
	defer ClearSession("t-rr")
	ctx, cancel := waitCtx()
	defer cancel()
	start := time.Now()
	if err := g.WaitFrozen(ctx, "t-rr"); err != nil {
		t.Fatalf("remote resume must release: %v", err)
	}
	if el := time.Since(start); el < 500*time.Millisecond {
		t.Fatalf("远端挂起应至少经历一个轮询周期，实际 %v", el)
	}
	if atomic.LoadInt32(&freezeCount) != 1 {
		t.Fatalf("freeze callback must fire exactly once, got %d", freezeCount)
	}
}

// 本地 gate Pause（快路径）→远端持续 PAUSED→本地 Resume 放行（本地通道解除）。
func TestWaitFrozen_LocalPauseLocalResume(t *testing.T) {
	restore := stubTaskState(func() string { return "TASK_STATUS_PAUSED" })
	defer restore()
	OnFrozen("t-ll", func() {})
	ClearFrozen("t-ll")
	g := GateFor("t-ll")
	defer ClearSession("t-ll")
	PauseSession("t-ll")
	done := make(chan error, 1)
	go func() { done <- g.WaitFrozen(context.Background(), "t-ll") }()
	time.Sleep(200 * time.Millisecond)
	ResumeSession("t-ll")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("local resume must release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local resume did not release gate")
	}
}

// 远端 PAUSED→本地 gate 未动→远端解除即放行（跨副本语义：非属主副本暂停也能解）。
func TestWaitFrozen_RemotePauseNoLocalGate(t *testing.T) {
	restore := stubTaskState(func() string { return "TASK_STATUS_PAUSED" }, func() string { return "TASK_STATUS_COMPLETED" })
	defer restore()
	OnFrozen("t-rn", func() {})
	ClearFrozen("t-rn")
	g := GateFor("t-rn") // 本地从未 Pause
	defer ClearSession("t-rn")
	ctx, cancel := waitCtx()
	defer cancel()
	if err := g.WaitFrozen(ctx, "t-rn"); err != nil {
		t.Fatalf("remote-only pause/resume must work without local gate: %v", err)
	}
}

// 双通道均清=直通零等待（现网主路径零开销）。
func TestWaitFrozen_BothClearFastPath(t *testing.T) {
	restore := stubTaskState(func() string { return "TASK_STATUS_RUNNING" })
	defer restore()
	g := GateFor("t-fc")
	defer ClearSession("t-fc")
	start := time.Now()
	ctx, cancel := waitCtx()
	defer cancel()
	if err := g.WaitFrozen(ctx, "t-fc"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("clear path must be near-instant, took %v", el)
	}
}
