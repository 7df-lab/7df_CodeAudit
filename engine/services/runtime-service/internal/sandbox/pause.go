// PauseGate — AI 交互会话的回合闸门（ADR-200）。
//
// 暂停语义（诚实边界）: 已发给沙箱 DSH 的当前回合无法中途暂停（模型流不可回退），
// 闸门在**回合边界**生效——当前回合跑完、下回合开始前挂起；恢复后继续后续回合。
// 注册表为包级共享：runner 每次 newSandboxRunner 都是新实例（verify/missed/五角色
// 各自建 runner），闸门必须跨实例按 task_id 寻址。
package sandbox

import (
	"context"
	"strings"
	"sync"
	"time"
)

// ---- 回合派生寻址----

// gateKey — 回合闸门寻址键：剥回合型派生后缀归一根任务 ID。PauseTask 挂在任务
// 实体上，补丁回合（TaskID+"-fix"，sandbox_analysis patchRound）必须共享主任务
// 的闸门与冻结锚——独立寻址=PAUSED 态照常开新沙箱跑完+冻结回调 miss（权威面
// 查询 -fix 实体亦 NotFound，双通道全失效）。ClearSession 仍用原始 t.TaskID
// （派生键清理=no-op 无害；根键 gate 由主回合 Run 的 defer 回收，不提前清）。
func gateKey(taskID string) string {
	if s, ok := strings.CutSuffix(taskID, "-fix"); ok && s != "" {
		return s
	}
	return taskID
}

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
		pauses[taskID] = g
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

// ---- 冻结回调注册表（R2：gate 冻结点的换手基线锚，TP17-T2 / ADR-238）----

var (
	freezesMu sync.Mutex
	freezes   = map[string]func(){} // taskID → 冻结回调（service 层注册的闭包）
)

// OnFrozen — 注册冻结回调（service 层在建立持笔上下文后调用；同 taskID 重复
// 注册后者覆盖——retryFailedPatches 等多 Run 场景以最后一次注册为准）。
func OnFrozen(taskID string, fn func()) {
	freezesMu.Lock()
	defer freezesMu.Unlock()
	freezes[taskID] = fn
}

// ClearFrozen — 注销冻结回调（与 Run 同生命周期：service 层 defer 释放，
// 不挂在 ClearSession——回调闭包依赖 service 层状态，注册方负责回收）。
func ClearFrozen(taskID string) {
	freezesMu.Lock()
	defer freezesMu.Unlock()
	delete(freezes, taskID)
}

// TaskStateFunc — 任务状态查询（HA P1-b ADR-250 上收双通道的权威路径：service
// 层装配注入，指向 task-service GetScanTask；返回任务状态字符串或错误）。
// nil=未装配（纯本地 gate 语义，现网单副本口径）。
var TaskStateFunc func(ctx context.Context, taskID string) (string, error)

// pausePollInterval — 权威路径轮询周期（人机操作语义，秒级无感——设计档 §1 Q9 契约）。
const pausePollInterval = time.Second

// taskPausedRemotely — 权威路径判定：任务状态机目标态为 PAUSED。
func taskPausedRemotely(ctx context.Context, taskID string) bool {
	if TaskStateFunc == nil {
		return false
	}
	st, err := TaskStateFunc(ctx, taskID)
	if err != nil {
		return false // task-service 不可达：本地 gate 语义兜底（快路径已判过）
	}
	return strings.Contains(st, "PAUSED")
}

// remoteResume — 权威路径解除判定：任务状态离开 PAUSED（或查询面不可达=解除，
// 避免编排面故障把执行器永久挂死——诚实降级）。
func remoteResume(ctx context.Context, taskID string) bool {
	if TaskStateFunc == nil {
		return true
	}
	st, err := TaskStateFunc(ctx, taskID)
	if err != nil {
		return true
	}
	return !strings.Contains(st, "PAUSED")
}

// WaitFrozen — 同 Wait，且在确认进入挂起时先同步执行冻结回调（换手基线锚，
// R2：此刻回合已收束、SSE 已定格——Run 收束 defer 投影对中途冻结不生效，
// 必须在真实冻结点重投影+锚定）。回调失败不阻断挂起本身（降级在闭包内）。
// 已知竞态（可接受，ADR-238）：检查 paused 与回调执行间 Resume 到达 → 回调
// 多跑一次，锚定内容幂等（baton handoff 防重），无正确性影响。
//
// HA P1-b 双通道（ADR-250）：本地 gate（快路径——task-service 闸门先行的
// dshControl 打中本副本时立即生效）OR 任务状态机 PAUSED（权威路径——跨副本：
// 暂停请求落到任意副本翻状态后，属主副本在回合边界轮询感知）。挂起期间按
// pausePollInterval 轮询权威面，本地 gate Resume 与远端离开 PAUSED 任一解除
// 即放行（双保险：两条路径都能解）。
func (g *PauseGate) WaitFrozen(ctx context.Context, taskID string) error {
	freeze := func() {
		freezesMu.Lock()
		fn := freezes[taskID]
		freezesMu.Unlock()
		if fn != nil {
			fn()
		}
	}
	frozen := false // 冻结回调恰一次（本地/远端任一通道首次判定挂起时）
	freezeOnce := func() {
		if !frozen {
			frozen = true
			freeze()
		}
	}
	g.mu.Lock()
	localPaused := g.paused
	g.mu.Unlock()
	if !localPaused && !taskPausedRemotely(ctx, taskID) {
		return nil // 双通道均未挂起：直接过闸
	}
	freezeOnce()
	t := time.NewTicker(pausePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.wakeCh():
			g.mu.Lock()
			p := g.paused
			g.mu.Unlock()
			if !p {
				return nil // 本地通道解除
			}
		case <-t.C:
			if remoteResume(ctx, taskID) {
				return nil // 权威通道解除
			}
		}
	}
}

// wakeCh — 本地唤醒通道快照（close 广播语义；gate 未暂停时等待方应已放行，
// 此处仅防御性读取）。
func (g *PauseGate) wakeCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wake
}

// ClearSession — 会话结束清理（防泄漏；挂起中的 gate 一并清除，任务已被回收）。
func ClearSession(taskID string) {
	pausesMu.Lock()
	defer pausesMu.Unlock()
	delete(pauses, taskID)
}
