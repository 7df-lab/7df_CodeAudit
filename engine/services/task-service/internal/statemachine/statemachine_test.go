package statemachine

import (
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 依据: 04_工作流设计.md §1 统一任务状态机
// 测试覆盖: 每个合法转换 + 每个非法转换拒绝

func TestStateMachine_ValidTransitions(t *testing.T) {
	sm := New()

	tests := []struct {
		name string
		from TaskStatus
		to   TaskStatus
		rpc  string
	}{
		// T4: QUEUED → RUNNING (StartTask)
		{
			name: "T4: QUEUED -> RUNNING via StartTask",
			from: TaskStatusQueued,
			to:   TaskStatusRunning,
			rpc:  "StartTask",
		},
		// T5: RUNNING → COMPLETED (CompleteTask)
		{
			name: "T5: RUNNING -> COMPLETED via CompleteTask",
			from: TaskStatusRunning,
			to:   TaskStatusCompleted,
			rpc:  "CompleteTask",
		},
		// T6: RUNNING → FAILED (FailTask)
		{
			name: "T6: RUNNING -> FAILED via FailTask",
			from: TaskStatusRunning,
			to:   TaskStatusFailed,
			rpc:  "FailTask",
		},
		// T7: FAILED → QUEUED (AutoRetry)
		{
			name: "T7: FAILED -> QUEUED via AutoRetry",
			from: TaskStatusFailed,
			to:   TaskStatusQueued,
			rpc:  "AutoRetry",
		},
		// T8: FAILED → DEAD (RetryExhausted)
		{
			name: "T8: FAILED -> DEAD via RetryExhausted",
			from: TaskStatusFailed,
			to:   TaskStatusDead,
			rpc:  "RetryExhausted",
		},
		// T9: RUNNING → TIMEOUT (ReconcileTimeout)
		{
			name: "T9: RUNNING -> TIMEOUT via ReconcileTimeout",
			from: TaskStatusRunning,
			to:   TaskStatusTimeout,
			rpc:  "ReconcileTimeout",
		},
		// T10: * → CANCELLED (CancelScanTask)
		{
			name: "T10a: CREATED -> CANCELLED via CancelScanTask",
			from: TaskStatusCreated,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10b: PENDING -> CANCELLED via CancelScanTask",
			from: TaskStatusPending,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10c: QUEUED -> CANCELLED via CancelScanTask",
			from: TaskStatusQueued,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10d: RUNNING -> CANCELLED via CancelScanTask",
			from: TaskStatusRunning,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10e: FAILED -> CANCELLED via CancelScanTask",
			from: TaskStatusFailed,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10f: TIMEOUT -> CANCELLED via CancelScanTask",
			from: TaskStatusTimeout,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
		{
			name: "T10g: DEAD -> CANCELLED via CancelScanTask",
			from: TaskStatusDead,
			to:   TaskStatusCancelled,
			rpc:  "CancelScanTask",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 验证 CanTransition
			if !sm.CanTransition(tt.from, tt.to) {
				t.Errorf("CanTransition(%v, %v) = false, want true", tt.from, tt.to)
			}

			// 验证 ValidateTransition 不返回错误
			if err := sm.ValidateTransition(tt.from, tt.to); err != nil {
				t.Errorf("ValidateTransition(%v, %v) returned error: %v", tt.from, tt.to, err)
			}

			// 验证 RPC 名称
			rpc, ok := sm.GetTransitionRPC(tt.from, tt.to)
			if !ok {
				t.Errorf("GetTransitionRPC(%v, %v) ok = false", tt.from, tt.to)
			}
			if rpc != tt.rpc {
				t.Errorf("GetTransitionRPC(%v, %v) = %q, want %q", tt.from, tt.to, rpc, tt.rpc)
			}
		})
	}
}

func TestStateMachine_InvalidTransitions(t *testing.T) {
	sm := New()

	tests := []struct {
		name string
		from TaskStatus
		to   TaskStatus
	}{
		// ADR-171: CREATED → RUNNING 合法（审批流废除）；只能到 RUNNING 或 CANCELLED
		{
			name: "CREATED -> QUEUED (invalid)",
			from: TaskStatusCreated,
			to:   TaskStatusQueued,
		},
		{
			name: "CREATED -> COMPLETED (invalid)",
			from: TaskStatusCreated,
			to:   TaskStatusCompleted,
		},
		{
			name: "CREATED -> FAILED (invalid)",
			from: TaskStatusCreated,
			to:   TaskStatusFailed,
		},
		{
			name: "CREATED -> TIMEOUT (invalid)",
			from: TaskStatusCreated,
			to:   TaskStatusTimeout,
		},
		{
			name: "CREATED -> DEAD (invalid)",
			from: TaskStatusCreated,
			to:   TaskStatusDead,
		},

		// PENDING 只能到 QUEUED, FAILED, CANCELLED
		{
			name: "PENDING -> CREATED (invalid)",
			from: TaskStatusPending,
			to:   TaskStatusCreated,
		},
		{
			name: "PENDING -> RUNNING (invalid)",
			from: TaskStatusPending,
			to:   TaskStatusRunning,
		},
		{
			name: "PENDING -> COMPLETED (invalid)",
			from: TaskStatusPending,
			to:   TaskStatusCompleted,
		},
		{
			name: "PENDING -> TIMEOUT (invalid)",
			from: TaskStatusPending,
			to:   TaskStatusTimeout,
		},
		{
			name: "PENDING -> DEAD (invalid)",
			from: TaskStatusPending,
			to:   TaskStatusDead,
		},

		// QUEUED 只能到 RUNNING 或 CANCELLED
		{
			name: "QUEUED -> CREATED (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusCreated,
		},
		{
			name: "QUEUED -> PENDING (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusPending,
		},
		{
			name: "QUEUED -> COMPLETED (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusCompleted,
		},
		{
			name: "QUEUED -> FAILED (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusFailed,
		},
		{
			name: "QUEUED -> TIMEOUT (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusTimeout,
		},
		{
			name: "QUEUED -> DEAD (invalid)",
			from: TaskStatusQueued,
			to:   TaskStatusDead,
		},

		// RUNNING 只能到 COMPLETED, FAILED, TIMEOUT, CANCELLED
		{
			name: "RUNNING -> CREATED (invalid)",
			from: TaskStatusRunning,
			to:   TaskStatusCreated,
		},
		{
			name: "RUNNING -> PENDING (invalid)",
			from: TaskStatusRunning,
			to:   TaskStatusPending,
		},
		{
			name: "RUNNING -> QUEUED (invalid)",
			from: TaskStatusRunning,
			to:   TaskStatusQueued,
		},
		{
			name: "RUNNING -> DEAD (invalid)",
			from: TaskStatusRunning,
			to:   TaskStatusDead,
		},

		// FAILED 只能到 QUEUED, DEAD, CANCELLED
		{
			name: "FAILED -> CREATED (invalid)",
			from: TaskStatusFailed,
			to:   TaskStatusCreated,
		},
		{
			name: "FAILED -> PENDING (invalid)",
			from: TaskStatusFailed,
			to:   TaskStatusPending,
		},
		{
			name: "FAILED -> RUNNING (invalid)",
			from: TaskStatusFailed,
			to:   TaskStatusRunning,
		},
		{
			name: "FAILED -> COMPLETED (invalid)",
			from: TaskStatusFailed,
			to:   TaskStatusCompleted,
		},
		{
			name: "FAILED -> TIMEOUT (invalid)",
			from: TaskStatusFailed,
			to:   TaskStatusTimeout,
		},

		// COMPLETED 是终态，不能转出
		{
			name: "COMPLETED -> CREATED (invalid, terminal)",
			from: TaskStatusCompleted,
			to:   TaskStatusCreated,
		},
		{
			name: "COMPLETED -> RUNNING (invalid, terminal)",
			from: TaskStatusCompleted,
			to:   TaskStatusRunning,
		},
		{
			name: "COMPLETED -> CANCELLED (invalid, terminal)",
			from: TaskStatusCompleted,
			to:   TaskStatusCancelled,
		},

		// CANCELLED 是终态，不能转出
		{
			name: "CANCELLED -> CREATED (invalid, terminal)",
			from: TaskStatusCancelled,
			to:   TaskStatusCreated,
		},
		{
			name: "CANCELLED -> RUNNING (invalid, terminal)",
			from: TaskStatusCancelled,
			to:   TaskStatusRunning,
		},

		// TIMEOUT 只能到 CANCELLED
		{
			name: "TIMEOUT -> CREATED (invalid)",
			from: TaskStatusTimeout,
			to:   TaskStatusCreated,
		},
		{
			name: "TIMEOUT -> RUNNING (invalid)",
			from: TaskStatusTimeout,
			to:   TaskStatusRunning,
		},
		{
			name: "TIMEOUT -> COMPLETED (invalid)",
			from: TaskStatusTimeout,
			to:   TaskStatusCompleted,
		},

		// DEAD 只能到 CANCELLED
		{
			name: "DEAD -> CREATED (invalid)",
			from: TaskStatusDead,
			to:   TaskStatusCreated,
		},
		{
			name: "DEAD -> RUNNING (invalid)",
			from: TaskStatusDead,
			to:   TaskStatusRunning,
		},
		{
			name: "DEAD -> QUEUED (invalid, 04 §1 未显式画出此边)",
			from: TaskStatusDead,
			to:   TaskStatusQueued,
		},
		{
			name: "DEAD -> COMPLETED (invalid)",
			from: TaskStatusDead,
			to:   TaskStatusCompleted,
		},

		// UNSPECIFIED 不能作为源状态
		{
			name: "UNSPECIFIED -> CREATED (invalid)",
			from: TaskStatusUnspecified,
			to:   TaskStatusCreated,
		},
		{
			name: "UNSPECIFIED -> PENDING (invalid)",
			from: TaskStatusUnspecified,
			to:   TaskStatusPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 验证 CanTransition 返回 false
			if sm.CanTransition(tt.from, tt.to) {
				t.Errorf("CanTransition(%v, %v) = true, want false", tt.from, tt.to)
			}

			// 验证 ValidateTransition 返回 FailedPrecondition 错误
			err := sm.ValidateTransition(tt.from, tt.to)
			if err == nil {
				t.Errorf("ValidateTransition(%v, %v) = nil, want error", tt.from, tt.to)
				return
			}

			// 验证错误码是 FailedPrecondition
			st, ok := status.FromError(err)
			if !ok {
				t.Errorf("error is not a status error: %v", err)
				return
			}
			if st.Code() != codes.FailedPrecondition {
				t.Errorf("error code = %v, want FailedPrecondition", st.Code())
			}

			// 验证 RPC 名称不存在
			_, ok = sm.GetTransitionRPC(tt.from, tt.to)
			if ok {
				t.Errorf("GetTransitionRPC(%v, %v) ok = true, want false", tt.from, tt.to)
			}
		})
	}
}

func TestStateMachine_SelfTransition(t *testing.T) {
	sm := New()

	statuses := []TaskStatus{
		TaskStatusCreated, TaskStatusPending, TaskStatusQueued,
		TaskStatusRunning, TaskStatusCompleted, TaskStatusFailed,
		TaskStatusCancelled, TaskStatusTimeout, TaskStatusDead,
	}

	for _, s := range statuses {
		t.Run(s.String()+" -> "+s.String(), func(t *testing.T) {
			if sm.CanTransition(s, s) {
				t.Errorf("CanTransition(%v, %v) = true, self-transition should be invalid", s, s)
			}
		})
	}
}

func TestStateMachine_TerminalStates(t *testing.T) {
	sm := New()

	terminalStates := []TaskStatus{
		TaskStatusCompleted,
		TaskStatusCancelled,
		TaskStatusTimeout,
		TaskStatusDead,
	}

	for _, s := range terminalStates {
		t.Run(s.String()+" is terminal", func(t *testing.T) {
			if !sm.IsTerminal(s) {
				t.Errorf("IsTerminal(%v) = false, want true", s)
			}
		})
	}

	nonTerminalStates := []TaskStatus{
		TaskStatusUnspecified,
		TaskStatusCreated,
		TaskStatusPending,
		TaskStatusQueued,
		TaskStatusRunning,
		TaskStatusFailed,
	}

	for _, s := range nonTerminalStates {
		t.Run(s.String()+" is not terminal", func(t *testing.T) {
			if sm.IsTerminal(s) {
				t.Errorf("IsTerminal(%v) = true, want false", s)
			}
		})
	}
}

func TestStateMachine_GetAllowedTransitions(t *testing.T) {
	sm := New()

	tests := []struct {
		name     string
		from     TaskStatus
		expected int
	}{
		{
			name:     "CREATED has 2 transitions (RUNNING, CANCELLED)",
			from:     TaskStatusCreated,
			expected: 2,
		},
		{
			// PENDING 为保留值（审批流废除后不再产生），仅剩 T10 通配取消边
			name:     "PENDING has 1 transition (CANCELLED via T10; ADR-171 后不再产生)",
			from:     TaskStatusPending,
			expected: 1,
		},
		{
			name:     "QUEUED has 2 transitions (RUNNING, CANCELLED)",
			from:     TaskStatusQueued,
			expected: 2,
		},
		{
			name:     "RUNNING has 5 transitions (PAUSED, COMPLETED, FAILED, TIMEOUT, CANCELLED)",
			from:     TaskStatusRunning,
			expected: 5, // ADR-200: += PAUSED (PauseTask)
		},
		{
			name:     "FAILED has 3 transitions (QUEUED, DEAD, CANCELLED)",
			from:     TaskStatusFailed,
			expected: 3,
		},
		{
			name:     "TIMEOUT has 1 transition (CANCELLED)",
			from:     TaskStatusTimeout,
			expected: 1,
		},
		{
			name:     "DEAD has 1 transition (CANCELLED)",
			from:     TaskStatusDead,
			expected: 1,
		},
		{
			name:     "COMPLETED has 0 transitions (terminal)",
			from:     TaskStatusCompleted,
			expected: 0,
		},
		{
			name:     "CANCELLED has 0 transitions (terminal)",
			from:     TaskStatusCancelled,
			expected: 0,
		},
		{
			name:     "UNSPECIFIED has 0 transitions",
			from:     TaskStatusUnspecified,
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transitions := sm.GetAllowedTransitions(tt.from)
			if len(transitions) != tt.expected {
				t.Errorf("GetAllowedTransitions(%v) returned %d transitions, want %d: %v",
					tt.from, len(transitions), tt.expected, transitions)
			}
		})
	}
}

func TestStateMachine_CompleteFlow(t *testing.T) {
	sm := New()

	// 测试正常流程: CREATED → RUNNING → COMPLETED（ADR-171 审批流废除）
	flow := []struct {
		from TaskStatus
		to   TaskStatus
		rpc  string
	}{
		{TaskStatusCreated, TaskStatusRunning, "StartTask"},
		{TaskStatusRunning, TaskStatusCompleted, "CompleteTask"},
	}

	current := TaskStatusCreated
	for _, step := range flow {
		if !sm.CanTransition(current, step.to) {
			t.Fatalf("Cannot transition from %v to %v", current, step.to)
		}
		current = step.to
	}

	if current != TaskStatusCompleted {
		t.Errorf("Final state = %v, want COMPLETED", current)
	}
}

func TestStateMachine_FailedRetryFlow(t *testing.T) {
	sm := New()

	// 测试失败重试流程: RUNNING → FAILED → QUEUED → RUNNING → FAILED → QUEUED → RUNNING → FAILED → DEAD
	current := TaskStatusRunning

	// 第一次失败
	if !sm.CanTransition(current, TaskStatusFailed) {
		t.Fatal("Cannot transition RUNNING -> FAILED")
	}
	current = TaskStatusFailed

	// 第一次重试
	if !sm.CanTransition(current, TaskStatusQueued) {
		t.Fatal("Cannot transition FAILED -> QUEUED")
	}
	current = TaskStatusQueued

	// 再次运行
	if !sm.CanTransition(current, TaskStatusRunning) {
		t.Fatal("Cannot transition QUEUED -> RUNNING")
	}
	current = TaskStatusRunning

	// 第二次失败
	if !sm.CanTransition(current, TaskStatusFailed) {
		t.Fatal("Cannot transition RUNNING -> FAILED")
	}
	current = TaskStatusFailed

	// 第二次重试
	if !sm.CanTransition(current, TaskStatusQueued) {
		t.Fatal("Cannot transition FAILED -> QUEUED")
	}
	current = TaskStatusQueued

	// 再次运行
	if !sm.CanTransition(current, TaskStatusRunning) {
		t.Fatal("Cannot transition QUEUED -> RUNNING")
	}
	current = TaskStatusRunning

	// 第三次失败（重试耗尽）
	if !sm.CanTransition(current, TaskStatusFailed) {
		t.Fatal("Cannot transition RUNNING -> FAILED")
	}
	current = TaskStatusFailed

	// 转为 DEAD
	if !sm.CanTransition(current, TaskStatusDead) {
		t.Fatal("Cannot transition FAILED -> DEAD")
	}
	current = TaskStatusDead

	if current != TaskStatusDead {
		t.Errorf("Final state = %v, want DEAD", current)
	}

	// DEAD 是终态
	if !sm.IsTerminal(current) {
		t.Error("DEAD should be terminal")
	}
}

func TestStateMachine_ConcurrentAccess(t *testing.T) {
	sm := New()

	// 并发测试：多个 goroutine 同时读写
	done := make(chan bool, 100)

	for i := 0; i < 100; i++ {
		go func() {
			// 并发读取
			sm.CanTransition(TaskStatusCreated, TaskStatusPending)
			sm.GetAllowedTransitions(TaskStatusRunning)
			sm.IsTerminal(TaskStatusCompleted)
			sm.GetTransitionRPC(TaskStatusCreated, TaskStatusPending)
			done <- true
		}()
	}

	for i := 0; i < 100; i++ {
		<-done
	}
}

// TestTaskStatusProtoValues 验证我们的常量与 proto 定义一致
func TestTaskStatusProtoValues(t *testing.T) {
	tests := []struct {
		name     string
		local    TaskStatus
		expected pb.TaskStatus
	}{
		{"UNSPECIFIED", TaskStatusUnspecified, pb.TaskStatus_TASK_STATUS_UNSPECIFIED},
		{"CREATED", TaskStatusCreated, pb.TaskStatus_TASK_STATUS_CREATED},
		{"PENDING", TaskStatusPending, pb.TaskStatus_TASK_STATUS_PENDING},
		{"QUEUED", TaskStatusQueued, pb.TaskStatus_TASK_STATUS_QUEUED},
		{"RUNNING", TaskStatusRunning, pb.TaskStatus_TASK_STATUS_RUNNING},
		{"COMPLETED", TaskStatusCompleted, pb.TaskStatus_TASK_STATUS_COMPLETED},
		{"FAILED", TaskStatusFailed, pb.TaskStatus_TASK_STATUS_FAILED},
		{"CANCELLED", TaskStatusCancelled, pb.TaskStatus_TASK_STATUS_CANCELLED},
		{"TIMEOUT", TaskStatusTimeout, pb.TaskStatus_TASK_STATUS_TIMEOUT},
		{"DEAD", TaskStatusDead, pb.TaskStatus_TASK_STATUS_DEAD},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.local != tt.expected {
				t.Errorf("%s: local = %d, proto = %d", tt.name, tt.local, tt.expected)
			}
		})
	}
}
