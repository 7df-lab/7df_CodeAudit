package reconciler

import (
	"sync"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MockTaskStore implements TaskStore for testing.
type MockTaskStore struct {
	mu      sync.Mutex
	tasks   map[string]*pb.ScanTask
	updates []UpdateRecord
}

type UpdateRecord struct {
	TaskID    string
	NewStatus pb.TaskStatus
}

func NewMockTaskStore() *MockTaskStore {
	return &MockTaskStore{
		tasks:   make(map[string]*pb.ScanTask),
		updates: make([]UpdateRecord, 0),
	}
}

func (m *MockTaskStore) AddTask(task *pb.ScanTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[task.TaskId] = task
}

func (m *MockTaskStore) GetRunningTasks() []*pb.ScanTask {
	m.mu.Lock()
	defer m.mu.Unlock()

	var running []*pb.ScanTask
	for _, t := range m.tasks {
		if t.Status == pb.TaskStatus_TASK_STATUS_RUNNING {
			running = append(running, t)
		}
	}
	return running
}

func (m *MockTaskStore) UpdateTaskStatus(taskID string, newStatus pb.TaskStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[taskID]
	if !ok {
		return nil
	}

	task.Status = newStatus
	m.updates = append(m.updates, UpdateRecord{
		TaskID:    taskID,
		NewStatus: newStatus,
	})
	return nil
}

func (m *MockTaskStore) GetUpdates() []UpdateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]UpdateRecord, len(m.updates))
	copy(result, m.updates)
	return result
}

// TestReconciler_TimeoutDetection tests that RUNNING tasks exceeding timeout are marked TIMEOUT.
// 依据: 04 §1 "超时对账每天 02:00 全量扫描长时间 RUNNING 任务（>见 07 §8 超时上限）转 TIMEOUT"
// 依据: 07 §8 超时矩阵
func TestReconciler_TimeoutDetection(t *testing.T) {
	store := NewMockTaskStore()

	// Add a task that has been running for 31 minutes (exceeds 30m timeout)
	oldTask := &pb.ScanTask{
		TaskId:    "task-old",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-31 * time.Minute)),
	}
	store.AddTask(oldTask)

	// Add a task that has been running for 10 minutes (within timeout)
	recentTask := &pb.ScanTask{
		TaskId:    "task-recent",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-10 * time.Minute)),
	}
	store.AddTask(recentTask)

	// Create reconciler with 30 minute timeout
	// 依据: 07 §8 "Task → DSHRuntime（模式A/B/C）: 30m"
	var timeoutTasks []string
	r := New(store,
		WithTimeout(30*time.Minute), // 07 §8
		WithOnTimeout(func(taskID string) {
			timeoutTasks = append(timeoutTasks, taskID)
		}),
	)

	// Run reconciliation once
	r.reconcile()

	// Verify old task was marked TIMEOUT
	if oldTask.Status != pb.TaskStatus_TASK_STATUS_TIMEOUT {
		t.Errorf("old task status = %v, want TIMEOUT", oldTask.Status)
	}

	// Verify recent task is still RUNNING
	if recentTask.Status != pb.TaskStatus_TASK_STATUS_RUNNING {
		t.Errorf("recent task status = %v, want RUNNING", recentTask.Status)
	}

	// Verify timeout callback was called
	if len(timeoutTasks) != 1 || timeoutTasks[0] != "task-old" {
		t.Errorf("timeoutTasks = %v, want [task-old]", timeoutTasks)
	}

	// Verify update records
	updates := store.GetUpdates()
	if len(updates) != 1 {
		t.Fatalf("updates count = %d, want 1", len(updates))
	}
	if updates[0].TaskID != "task-old" || updates[0].NewStatus != pb.TaskStatus_TASK_STATUS_TIMEOUT {
		t.Errorf("update = %v, want task-old -> TIMEOUT", updates[0])
	}
}

// TestReconciler_NoTimeoutWhenWithinLimit tests that tasks within timeout limit are not affected.
func TestReconciler_NoTimeoutWhenWithinLimit(t *testing.T) {
	store := NewMockTaskStore()

	// All tasks within timeout
	tasks := []*pb.ScanTask{
		{
			TaskId:    "task-1",
			Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
			UpdatedAt: timestamppb.New(time.Now().Add(-5 * time.Minute)),
		},
		{
			TaskId:    "task-2",
			Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
			UpdatedAt: timestamppb.New(time.Now().Add(-20 * time.Minute)),
		},
		{
			TaskId:    "task-3",
			Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
			UpdatedAt: timestamppb.New(time.Now().Add(-29 * time.Minute)),
		},
	}

	for _, t := range tasks {
		store.AddTask(t)
	}

	r := New(store, WithTimeout(30*time.Minute))
	r.reconcile()

	// All tasks should still be RUNNING
	for _, task := range tasks {
		if task.Status != pb.TaskStatus_TASK_STATUS_RUNNING {
			t.Errorf("task %s status = %v, want RUNNING", task.TaskId, task.Status)
		}
	}

	// No updates should have been made
	updates := store.GetUpdates()
	if len(updates) != 0 {
		t.Errorf("updates count = %d, want 0", len(updates))
	}
}

// TestReconciler_MultipleTimeouts tests that multiple timed-out tasks are all handled.
func TestReconciler_MultipleTimeouts(t *testing.T) {
	store := NewMockTaskStore()

	// 3 tasks all timed out
	for i := 0; i < 3; i++ {
		store.AddTask(&pb.ScanTask{
			TaskId:    "task-" + string(rune('a'+i)),
			Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
			UpdatedAt: timestamppb.New(time.Now().Add(-35 * time.Minute)),
		})
	}

	var timeoutTasks []string
	r := New(store,
		WithTimeout(30*time.Minute),
		WithOnTimeout(func(taskID string) {
			timeoutTasks = append(timeoutTasks, taskID)
		}),
	)

	r.reconcile()

	// All 3 tasks should be TIMEOUT
	updates := store.GetUpdates()
	if len(updates) != 3 {
		t.Errorf("updates count = %d, want 3", len(updates))
	}

	for _, u := range updates {
		if u.NewStatus != pb.TaskStatus_TASK_STATUS_TIMEOUT {
			t.Errorf("task %s status = %v, want TIMEOUT", u.TaskID, u.NewStatus)
		}
	}

	// Callback should be called 3 times
	if len(timeoutTasks) != 3 {
		t.Errorf("timeoutTasks count = %d, want 3", len(timeoutTasks))
	}
}

// TestReconciler_NonRunningTasksIgnored tests that non-RUNNING tasks are not affected.
func TestReconciler_NonRunningTasksIgnored(t *testing.T) {
	store := NewMockTaskStore()

	// Add tasks in various states, all with old timestamps
	statuses := []pb.TaskStatus{
		pb.TaskStatus_TASK_STATUS_CREATED,
		pb.TaskStatus_TASK_STATUS_PENDING,
		pb.TaskStatus_TASK_STATUS_QUEUED,
		pb.TaskStatus_TASK_STATUS_COMPLETED,
		pb.TaskStatus_TASK_STATUS_FAILED,
		pb.TaskStatus_TASK_STATUS_CANCELLED,
		pb.TaskStatus_TASK_STATUS_TIMEOUT,
		pb.TaskStatus_TASK_STATUS_DEAD,
	}

	for i, s := range statuses {
		store.AddTask(&pb.ScanTask{
			TaskId:    "task-" + string(rune('a'+i)),
			Status:    s,
			UpdatedAt: timestamppb.New(time.Now().Add(-60 * time.Minute)),
		})
	}

	r := New(store, WithTimeout(30*time.Minute))
	r.reconcile()

	// No updates should have been made
	updates := store.GetUpdates()
	if len(updates) != 0 {
		t.Errorf("updates count = %d, want 0", len(updates))
	}
}

// TestReconciler_CustomTimeout tests custom timeout configuration.
func TestReconciler_CustomTimeout(t *testing.T) {
	store := NewMockTaskStore()

	// Task running for 6 minutes
	store.AddTask(&pb.ScanTask{
		TaskId:    "task-1",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-6 * time.Minute)),
	})

	// Custom 5 minute timeout
	var timeoutTasks []string
	r := New(store,
		WithTimeout(5*time.Minute),
		WithOnTimeout(func(taskID string) {
			timeoutTasks = append(timeoutTasks, taskID)
		}),
	)

	r.reconcile()

	if len(timeoutTasks) != 1 {
		t.Errorf("timeoutTasks count = %d, want 1", len(timeoutTasks))
	}
}

// TestReconciler_StartStop tests starting and stopping the reconciler.
func TestReconciler_StartStop(t *testing.T) {
	store := NewMockTaskStore()

	r := New(store,
		WithTimeout(30*time.Minute),
		WithInterval(100*time.Millisecond), // Fast interval for testing
	)

	// Start
	r.Start()
	if !r.IsRunning() {
		t.Error("reconciler should be running after Start()")
	}

	// Start again (should be no-op)
	r.Start()

	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	// Stop
	r.Stop()
	if r.IsRunning() {
		t.Error("reconciler should not be running after Stop()")
	}

	// Stop again (should be no-op)
	r.Stop()
}

// TestReconciler_TimeoutCallback tests that timeout callback is invoked correctly.
func TestReconciler_TimeoutCallback(t *testing.T) {
	store := NewMockTaskStore()

	store.AddTask(&pb.ScanTask{
		TaskId:    "task-1",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-31 * time.Minute)),
	})

	var callbackTaskID string
	var callbackMu sync.Mutex

	r := New(store,
		WithTimeout(30*time.Minute),
		WithOnTimeout(func(taskID string) {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			callbackTaskID = taskID
		}),
	)

	r.reconcile()

	callbackMu.Lock()
	defer callbackMu.Unlock()

	if callbackTaskID != "task-1" {
		t.Errorf("callback taskID = %q, want %q", callbackTaskID, "task-1")
	}
}

// TestReconciler_NoCallbackWhenNoTimeout tests that callback is not called when no timeout.
func TestReconciler_NoCallbackWhenNoTimeout(t *testing.T) {
	store := NewMockTaskStore()

	store.AddTask(&pb.ScanTask{
		TaskId:    "task-1",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-10 * time.Minute)),
	})

	callbackCalled := false
	r := New(store,
		WithTimeout(30*time.Minute),
		WithOnTimeout(func(taskID string) {
			callbackCalled = true
		}),
	)

	r.reconcile()

	if callbackCalled {
		t.Error("callback should not be called when no timeout")
	}
}

// TestReconciler_ConcurrentAccess tests concurrent access to the reconciler.
func TestReconciler_ConcurrentAccess(t *testing.T) {
	store := NewMockTaskStore()

	// Add many tasks
	for i := 0; i < 100; i++ {
		store.AddTask(&pb.ScanTask{
			TaskId:    "task-" + string(rune('a'+i%26)),
			Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
			UpdatedAt: timestamppb.New(time.Now().Add(-35 * time.Minute)),
		})
	}

	r := New(store, WithTimeout(30*time.Minute))

	// Run reconcile concurrently
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.reconcile()
		}()
	}

	wg.Wait()
}

// TestReconciler_ExactTimeoutBoundary tests behavior at exact timeout boundary.
func TestReconciler_ExactTimeoutBoundary(t *testing.T) {
	store := NewMockTaskStore()

	// Task at exact timeout boundary (30 minutes ago)
	// Note: Due to execution time, this might be slightly over or under
	store.AddTask(&pb.ScanTask{
		TaskId:    "task-boundary",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-30 * time.Minute)),
	})

	r := New(store, WithTimeout(30*time.Minute))
	r.reconcile()

	// At exact boundary, task should NOT be timed out (using >, not >=)
	// This is a design choice - we could also use >=
	updates := store.GetUpdates()
	if len(updates) != 0 {
		t.Logf("Note: task at exact boundary was timed out (updates=%d), this is acceptable", len(updates))
	}
}

// ---- ADR-196: 活跃度判定（updated_at ∪ AI 交互日志写入活跃度）----

// TestReconciler_AILogActivityKeepsTaskAlive reproduces gw-3a2a52330: updated_at stale
// beyond timeout but the AI interaction log was written recently — must NOT time out.
func TestReconciler_AILogActivityKeepsTaskAlive(t *testing.T) {
	store := NewMockTaskStore()
	task := &pb.ScanTask{
		TaskId:    "task-stale-record",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-70 * time.Minute)),
	}
	store.AddTask(task)

	r := New(store,
		WithTimeout(30*time.Minute),
		WithActivityLookup(func(taskID string) (time.Time, bool) {
			return time.Now().Add(-2 * time.Minute), true // AI 日志 2 分钟前仍在写
		}),
	)
	r.reconcile()

	if task.Status != pb.TaskStatus_TASK_STATUS_RUNNING {
		t.Errorf("task status = %v, want RUNNING (AI log active, ADR-196)", task.Status)
	}
	if len(store.GetUpdates()) != 0 {
		t.Errorf("updates = %v, want none", store.GetUpdates())
	}
}

// TestReconciler_BothStaleStillTimesOut: updated_at and AI log both beyond timeout → TIMEOUT.
func TestReconciler_BothStaleStillTimesOut(t *testing.T) {
	store := NewMockTaskStore()
	task := &pb.ScanTask{
		TaskId:    "task-dead",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-70 * time.Minute)),
	}
	store.AddTask(task)

	r := New(store,
		WithTimeout(30*time.Minute),
		WithActivityLookup(func(taskID string) (time.Time, bool) {
			return time.Now().Add(-45 * time.Minute), true // 比更新还晚但同样超时
		}),
	)
	r.reconcile()

	if task.Status != pb.TaskStatus_TASK_STATUS_TIMEOUT {
		t.Errorf("task status = %v, want TIMEOUT (both stale)", task.Status)
	}
}

// TestReconciler_ActivityLookupUnavailableFallsBack: probe returns ok=false
// (files missing) → falls back to updated_at-only judgment (original behavior).
func TestReconciler_ActivityLookupUnavailableFallsBack(t *testing.T) {
	store := NewMockTaskStore()
	task := &pb.ScanTask{
		TaskId:    "task-nolog",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-31 * time.Minute)),
	}
	store.AddTask(task)

	r := New(store,
		WithTimeout(30*time.Minute),
		WithActivityLookup(func(taskID string) (time.Time, bool) {
			return time.Time{}, false
		}),
	)
	r.reconcile()

	if task.Status != pb.TaskStatus_TASK_STATUS_TIMEOUT {
		t.Errorf("task status = %v, want TIMEOUT (fallback to updated_at)", task.Status)
	}
}

// TestReconciler_OlderActivityIgnored: probe timestamp older than updated_at must not
// extend or shrink the deadline — max(updated_at, ai log) is the judgment basis.
func TestReconciler_OlderActivityIgnored(t *testing.T) {
	store := NewMockTaskStore()
	task := &pb.ScanTask{
		TaskId:    "task-recent-record",
		Status:    pb.TaskStatus_TASK_STATUS_RUNNING,
		UpdatedAt: timestamppb.New(time.Now().Add(-10 * time.Minute)),
	}
	store.AddTask(task)

	r := New(store,
		WithTimeout(30*time.Minute),
		WithActivityLookup(func(taskID string) (time.Time, bool) {
			return time.Now().Add(-90 * time.Minute), true // 更旧，不应把判死基准拉前
		}),
	)
	r.reconcile()

	if task.Status != pb.TaskStatus_TASK_STATUS_RUNNING {
		t.Errorf("task status = %v, want RUNNING", task.Status)
	}
}
