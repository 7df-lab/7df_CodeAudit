// PauseGate — AI 交互会话的回合闸门（ADR-200）。
//
// 暂停语义（诚实边界）: 已发给沙箱 DSH 的当前回合无法中途暂停（模型流不可回退），
// 闸门在**回合边界**生效——当前回合跑完、下回合开始前挂起；恢复后继续后续回合。
// 注册表为包级共享：runner 每次 newSandboxRunner 都是新实例（verify/missed/五角色
// 各自建 runner），闸门必须跨实例按 task_id 寻址。
package sandbox

import (
	"context"
	"sync"
)

// PauseGate — 单任务的暂停闸门。
type PauseGate struct {
	mu     sync.Mutex
	paused bool
	wake   chan struct{} // paused 时非 nil；Resume 时 close 唤醒全部等待者
}

// NewPauseGate — 初始未暂停。
func NewPauseGate() *PauseGate { return &PauseGate{wake: make(chan struct{})} }

// Pause — 置暂停（幂等；重复 Pause 不重复换 wake）。
func (g *PauseGate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		g.paused = true
		g.wake = make(chan struct{})
	}
}

// Resume — 解除暂停并唤醒全部等待者（幂等）。
func (g *PauseGate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.wake)
	}
}

// Paused — 当前是否挂起。
func (g *PauseGate) Paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Wait — 暂停中则阻塞直至恢复或 ctx 取消。
func (g *PauseGate) Wait(ctx context.Context) error {
	g.mu.Lock()
	wake := g.wake
	paused := g.paused
	g.mu.Unlock()
	if !paused {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	}
}

// ---- 包级注册表（跨 runner 实例按 task_id 寻址）----

var (
	pausesMu sync.Mutex
	pauses   = map[string]*PauseGate{}
)

// PauseSession — 挂起任务会话（无 gate 则预约一个已暂停的——先暂停后建会话也生效）。
func PauseSession(taskID string) {
	pausesMu.Lock()
	defer pausesMu.Unlock()
	g, ok := pauses[taskID]
	if !ok {
		g = NewPauseGate()
		g.Pause()
		pauses[taskID] = g
		return
	}
	g.Pause()
}

// ResumeSession — 释放任务会话闸门。
func ResumeSession(taskID string) {
	pausesMu.Lock()
	defer pausesMu.Unlock()
	if g, ok := pauses[taskID]; ok {
		g.Resume()
	}
}

// GateFor — RunSession 取闸门（无则注册未暂停的，保证 PauseSession 与会话启动
// 的任意时序都收敛到同一 gate）。
func GateFor(taskID string) *PauseGate {
	pausesMu.Lock()
	defer pausesMu.Unlock()
	g, ok := pauses[taskID]
	if !ok {
		g = NewPauseGate()
		pauses[taskID] = g
	}
	return g
}

// ClearSession — 会话结束清理（防泄漏；挂起中的 gate 一并清除，任务已被回收）。
func ClearSession(taskID string) {
	pausesMu.Lock()
	defer pausesMu.Unlock()
	delete(pauses, taskID)
}
