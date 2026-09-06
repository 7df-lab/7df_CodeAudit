// Package reconciler provides task reconciliation logic for timeout detection.
// 依据: 04 §1 L26 "超时对账每天 02:00 全量扫描长时间 RUNNING 任务"
// 依据: 07 §8 超时矩阵
package reconciler

import (
	"log"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

// 依据: 07 §8 超时矩阵
// Task → CodeAnalysis: 10m
// Task → SASTAdapter: 20m
// Task → DSHRuntime (模式A/B/C): 30m
// Task → DSHRuntime (模式D): 20m

const (
	// DefaultTaskTimeout is the default timeout for tasks.
	// 依据: 07 §8 "Task → DSHRuntime（模式A/B/C）: 30m"
	// 使用 30 分钟作为通用超时上限
	DefaultTaskTimeout = 30 * time.Minute // 07 §8

	// ReconcileInterval is how often the reconciler scans for timed-out tasks.
	// 依据: 04 §1 "超时对账每小时"
	ReconcileInterval = 1 * time.Hour // 04 §1

	// DailyReconcileTime is when the daily full reconciliation runs.
	// 依据: 04 §1 L26 "每天 02:00 全量扫描"
	DailyReconcileTime = "02:00" // 04 §1
)

// TaskStore provides access to tasks for reconciliation.
type TaskStore interface {
	// GetRunningTasks returns all tasks in RUNNING state.
	GetRunningTasks() []*pb.ScanTask
	// UpdateTaskStatus updates the status of a task.
	UpdateTaskStatus(taskID string, newStatus pb.TaskStatus) error
}

// Reconciler periodically checks for timed-out tasks.
type Reconciler struct {
	store     TaskStore
	timeout   time.Duration
	interval  time.Duration
	stopCh    chan struct{}
	mu        sync.Mutex
	running   bool
	onTimeout func(taskID string) // callback for timeout handling (e.g., Saga compensation)

	// activityLookup 返回任务 AI 交互日志的最近写入时间（ADR-196）。
	// ok=false 表示不可用（探针故障/文件缺失），此时回退纯 updated_at 判定。
	activityLookup func(taskID string) (time.Time, bool)
}

// New creates a new Reconciler.
func New(store TaskStore, opts ...Option) *Reconciler {
	r := &Reconciler{
		store:    store,
		timeout:  DefaultTaskTimeout, // 07 §8
		interval: ReconcileInterval,  // 04 §1
		stopCh:   make(chan struct{}),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Option configures the Reconciler.
type Option func(*Reconciler)

// WithTimeout sets a custom timeout duration.
func WithTimeout(d time.Duration) Option {
	return func(r *Reconciler) {
		r.timeout = d
	}
}

// WithInterval sets a custom reconcile interval.
func WithInterval(d time.Duration) Option {
	return func(r *Reconciler) {
		r.interval = d
	}
}

// WithActivityLookup sets the AI-interaction-log activity probe (ADR-196).
// 依据: 人类裁决 2026-09-04——updated_at 与 AI 交互日志写入活跃度任一活跃即视为任务仍在 RUNNING
// （gw-3a2a52330 实证：AI 回合正常收敛前 3 秒被 updated_at 陈旧误杀）。
func WithActivityLookup(fn func(taskID string) (time.Time, bool)) Option {
	return func(r *Reconciler) {
		r.activityLookup = fn
	}
}

// WithOnTimeout sets a callback function to be called when a task times out.
func WithOnTimeout(fn func(taskID string)) Option {
	return func(r *Reconciler) {
		r.onTimeout = fn
	}
}

// Start begins the reconciliation loop.
func (r *Reconciler) Start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	go r.run()
	log.Printf("Reconciler started with timeout=%v interval=%v", r.timeout, r.interval)
}

// Stop stops the reconciliation loop.
func (r *Reconciler) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.running {
		return
	}

	close(r.stopCh)
	r.running = false
	log.Println("Reconciler stopped")
}

// IsRunning returns whether the reconciler is running.
func (r *Reconciler) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// run is the main reconciliation loop.
func (r *Reconciler) run() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Run immediately on start
	r.reconcile()

	for {
		select {
		case <-ticker.C:
			r.reconcile()
		case <-r.stopCh:
			return
		}
	}
}

// ReconcileOnce runs a single reconcile pass and returns the number of timed-out tasks.
// 依据: 04 §1 L26 超时对账；单次执行入口供单测与运维手动触发（ADR-131 接线）。
func (r *Reconciler) ReconcileOnce() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconcile()
}

// reconcile scans for timed-out tasks and marks them as TIMEOUT.
// 依据: 04 §1 "超时对账每天 02:00 全量扫描长时间 RUNNING 任务（>见 07 §8 超时上限）转 TIMEOUT"
func (r *Reconciler) reconcile() int {
	log.Println("Running reconciliation...")

	tasks := r.store.GetRunningTasks()
	now := time.Now()
	timedOut := 0

	for _, task := range tasks {
		// Check if task has been running too long
		// 依据: 07 §8 超时矩阵；ADR-196——updated_at ∪ AI 交互日志写入活跃度，任一活跃不判死
		lastActivity := task.GetUpdatedAt().AsTime()
		aliveVia := "updated_at"
		if r.activityLookup != nil {
			if ts, ok := r.activityLookup(task.TaskId); ok && ts.After(lastActivity) {
				lastActivity = ts
				aliveVia = "ai_interaction_log"
			}
		}
		if now.Sub(lastActivity) > r.timeout {
			log.Printf("Task %s timed out (last activity %v via %s, timeout=%v)",
				task.TaskId, lastActivity, aliveVia, r.timeout)

			// Mark as TIMEOUT
			// 依据: 04 §1 T9: RUNNING → TIMEOUT (ReconcileTimeout)
			if err := r.store.UpdateTaskStatus(task.TaskId, pb.TaskStatus_TASK_STATUS_TIMEOUT); err != nil {
				log.Printf("Failed to mark task %s as TIMEOUT: %v", task.TaskId, err)
				continue
			}
			timedOut++

			// Trigger timeout callback (e.g., Saga compensation)
			if r.onTimeout != nil {
				r.onTimeout(task.TaskId)
			}
		} else if aliveVia == "ai_interaction_log" {
			// ADR-196: updated_at 已陈旧但 AI 交互日志仍在写入——如实披露保活依据
			log.Printf("Task %s kept RUNNING: AI interaction log active (updated_at stale since %v)",
				task.TaskId, task.GetUpdatedAt().AsTime())
		}
	}
	return timedOut
}
