// WaitFrozen 冻结回调单测（R2/TP17-T2/ADR-238）：挂起进入时同步执行、
// 未挂起零开销不触发、注册覆盖与释放后防误触。
package sandbox

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pauseAndAwait — 挂起 gate 并在回调执行后释放（后台 Resume，避免测试阻塞）。
func pauseAndAwait(t *testing.T, g *PauseGate, taskID string) {
	t.Helper()
	g.Pause()
	go func() {
		time.Sleep(50 * time.Millisecond) // 给 WaitFrozen 进入挂起+执行回调的窗口
		g.Resume()
	}()
}

func TestWaitFrozenFiresOnPaused(t *testing.T) {
	const task = "T-frz-1"
	ClearSession(task)
	ClearFrozen(task)
	defer ClearSession(task)
	defer ClearFrozen(task)

	var calls int32
	OnFrozen(task, func() { atomic.AddInt32(&calls, 1) })
	g := GateFor(task)
	ctx := context.Background()

	// 未挂起：不触发回调，直通返回
	if err := g.WaitFrozen(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("未挂起不应触发回调, got %d", n)
	}

	// 挂起：回调同步执行恰一次，随后阻塞至 Resume
	pauseAndAwait(t, g, task)
	if err := g.WaitFrozen(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("挂起进入应恰好触发一次回调, got %d", n)
	}
}

func TestWaitFrozenOverwriteAndRelease(t *testing.T) {
	const task = "T-frz-2"
	ClearSession(task)
	ClearFrozen(task)
	defer ClearSession(task)
	defer ClearFrozen(task)

	var mu sync.Mutex
	first, second := 0, 0
	OnFrozen(task, func() { mu.Lock(); first++; mu.Unlock() })
	OnFrozen(task, func() { mu.Lock(); second++; mu.Unlock() }) // 覆盖注册（多 Run 场景）

	g := GateFor(task)
	pauseAndAwait(t, g, task)
	if err := g.WaitFrozen(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	f, s := first, second
	mu.Unlock()
	if f != 0 || s != 1 {
		t.Errorf("覆盖注册应只执行后者: first=%d second=%d", f, s)
	}

	// 释放后：再次挂起不得执行任何回调（注册表已清）
	ClearFrozen(task)
	g.Pause()
	go func() { time.Sleep(30 * time.Millisecond); g.Resume() }()
	if err := g.WaitFrozen(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	f, s = first, second
	mu.Unlock()
	if f != 0 || s != 1 {
		t.Errorf("释放后不得执行回调: first=%d second=%d", f, s)
	}
}
