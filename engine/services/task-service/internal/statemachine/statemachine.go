package statemachine

import (
	"fmt"
	"sync"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 依据: 04_工作流设计.md §1 统一任务状态机
// 依据: codeaudit_common.proto L167-L178 TaskStatus 枚举
// 依据: .agent/research/brief-TP03-04-05.md §3.2 状态机转换边表

// TaskStatus is a type alias for proto TaskStatus
type TaskStatus = pb.TaskStatus

// Task状态常量
const (
	TaskStatusUnspecified TaskStatus = pb.TaskStatus_TASK_STATUS_UNSPECIFIED
	TaskStatusCreated     TaskStatus = pb.TaskStatus_TASK_STATUS_CREATED
	TaskStatusPending     TaskStatus = pb.TaskStatus_TASK_STATUS_PENDING
	TaskStatusQueued      TaskStatus = pb.TaskStatus_TASK_STATUS_QUEUED
	TaskStatusRunning     TaskStatus = pb.TaskStatus_TASK_STATUS_RUNNING
	TaskStatusCompleted   TaskStatus = pb.TaskStatus_TASK_STATUS_COMPLETED
	TaskStatusFailed      TaskStatus = pb.TaskStatus_TASK_STATUS_FAILED
	TaskStatusCancelled   TaskStatus = pb.TaskStatus_TASK_STATUS_CANCELLED
	TaskStatusTimeout     TaskStatus = pb.TaskStatus_TASK_STATUS_TIMEOUT
	TaskStatusDead        TaskStatus = pb.TaskStatus_TASK_STATUS_DEAD
	TaskStatusPaused      TaskStatus = pb.TaskStatus_TASK_STATUS_PAUSED
)

// Transition 表示一个状态转换
type Transition struct {
	From TaskStatus
	To   TaskStatus
	RPC  string // 触发此转换的 RPC 名称
}

// StateMachine 管理任务状态转换
type StateMachine struct {
	mu sync.RWMutex
	// transitions[from] = set of allowed 'to' statuses
	transitions map[TaskStatus]map[TaskStatus]string
}

// New 创建新的状态机实例
// 依据: 04_工作流设计.md §1 状态机图 + brief-TP03-04-05.md §3.2 转换边表
func New() *StateMachine {
	sm := &StateMachine{
		transitions: make(map[TaskStatus]map[TaskStatus]string),
	}
	sm.registerTransitions()
	return sm
}

// registerTransitions 注册所有合法的状态转换
// 依据: 04_工作流设计.md §1 状态机图
// 依据: .agent/research/brief-TP03-04-05.md §3.2 全部合法转换边
func (sm *StateMachine) registerTransitions() {
	// T1(2026-09-01 人类裁定): CREATED → RUNNING (StartTask) —— 审批流（提交/批准/拒绝）
	// 废除，有创建权限即有启动权限；04 §1 的人工门三状态不再产生（PENDING/QUEUED 仅作
	// 存量兼容值，QUEUED 保留为自动重试目标态）
	sm.addTransition(TaskStatusCreated, TaskStatusRunning, "StartTask")

	// T4: QUEUED → RUNNING (StartTask) —— 自动重试路径（FAILED→QUEUED→RUNNING）
	sm.addTransition(TaskStatusQueued, TaskStatusRunning, "StartTask")

	// T5: RUNNING → COMPLETED (CompleteTask)
	sm.addTransition(TaskStatusRunning, TaskStatusCompleted, "CompleteTask")

	// T6: RUNNING → FAILED (FailTask)
	sm.addTransition(TaskStatusRunning, TaskStatusFailed, "FailTask")

	// T7: FAILED → QUEUED (自动重试)
	// 依据: proto L174 "FAILED→QUEUED 自动重试≤2次"
	sm.addTransition(TaskStatusFailed, TaskStatusQueued, "AutoRetry")

	// T8: FAILED → DEAD (重试耗尽)
	// 依据: proto L177 "重试耗尽，等待人工处理"
	sm.addTransition(TaskStatusFailed, TaskStatusDead, "RetryExhausted")

	// T9: RUNNING → TIMEOUT (超时对账)
	// 依据: 04 §1 L26 "超时对账" + 07 §8 超时矩阵
	sm.addTransition(TaskStatusRunning, TaskStatusTimeout, "ReconcileTimeout")

	// T10: * → CANCELLED (任何状态可取消)
	// 依据: 04 §1 "CANCELLED（任何状态可取消）"
	// 注意：DEAD → CANCELLED 不在原始状态机图中，但"任何状态"包含 DEAD
	// 依据: brief-TP03-04-05.md §3.2 #T10 注明 "*（任何状态）→ CANCELLED"
	allStatuses := []TaskStatus{
		TaskStatusCreated, TaskStatusPending, TaskStatusQueued,
		TaskStatusRunning, TaskStatusFailed, TaskStatusTimeout, TaskStatusDead,
	}
	for _, s := range allStatuses {
		sm.addTransition(s, TaskStatusCancelled, "CancelScanTask")
	}

	// T11: RUNNING → PAUSED (PauseTask, ADR-200) —— AI 交互会话回合闸门挂起
	sm.addTransition(TaskStatusRunning, TaskStatusPaused, "PauseTask")

	// T12: PAUSED → RUNNING (ResumeTask, ADR-200) —— 会话继续
	sm.addTransition(TaskStatusPaused, TaskStatusRunning, "ResumeTask")

	// PAUSED → CANCELLED（"任何状态可取消" T10 口径的延伸）
	sm.addTransition(TaskStatusPaused, TaskStatusCancelled, "CancelScanTask")

	// 歧义报告 (E2): 04 §1 状态机图未显式画出 DEAD → QUEUED 边
	// 但 proto L863 注释 "DEAD状态人工重试入口" 表明 RetryScanTask 可将 DEAD → QUEUED
	// brief-TP03-04-05.md §3.2 #T8 注意项也提到此歧义
	// 按设计文档字面实现：不添加 DEAD → QUEUED 边
	// DEAD 是终态，RetryScanTask 的语义需要人工确认
}

// addTransition 添加一条合法转换
func (sm *StateMachine) addTransition(from, to TaskStatus, rpc string) {
	if sm.transitions[from] == nil {
		sm.transitions[from] = make(map[TaskStatus]string)
	}
	sm.transitions[from][to] = rpc
}

// CanTransition 检查从 from 到 to 的转换是否合法
func (sm *StateMachine) CanTransition(from, to TaskStatus) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// UNSPECIFIED 不能作为源状态
	if from == TaskStatusUnspecified {
		return false
	}

	// 转换到自身是不允许的（除了 CancelScanTask 对已取消的任务是幂等的）
	if from == to {
		return false
	}

	allowed, exists := sm.transitions[from]
	if !exists {
		return false
	}

	_, ok := allowed[to]
	return ok
}

// GetTransitionRPC 获取触发此转换的 RPC 名称
func (sm *StateMachine) GetTransitionRPC(from, to TaskStatus) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	allowed, exists := sm.transitions[from]
	if !exists {
		return "", false
	}

	rpc, ok := allowed[to]
	return rpc, ok
}

// ValidateTransition 验证状态转换，非法转换返回 FailedPrecondition 错误
// 依据: 03_接口规范.md §3 错误码表 "任务状态不允许该操作 → FAILED_PRECONDITION(9)"
func (sm *StateMachine) ValidateTransition(from, to TaskStatus) error {
	if !sm.CanTransition(from, to) {
		return status.Errorf(codes.FailedPrecondition,
			"invalid state transition: %s → %s", from.String(), to.String())
	}
	return nil
}

// GetAllowedTransitions 获取当前状态的所有合法转换
func (sm *StateMachine) GetAllowedTransitions(from TaskStatus) []TaskStatus {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	allowed, exists := sm.transitions[from]
	if !exists {
		return nil
	}

	result := make([]TaskStatus, 0, len(allowed))
	for to := range allowed {
		result = append(result, to)
	}
	return result
}

// IsTerminal 检查状态是否为终态
// 终态: COMPLETED, CANCELLED, TIMEOUT, DEAD
func (sm *StateMachine) IsTerminal(s TaskStatus) bool {
	switch s {
	case TaskStatusCompleted, TaskStatusCancelled, TaskStatusTimeout, TaskStatusDead:
		return true
	default:
		return false
	}
}

// String 返回状态机的完整转换表（用于调试）
func (sm *StateMachine) String() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := "StateMachine Transitions:\n"
	for from, targets := range sm.transitions {
		for to, rpc := range targets {
			result += fmt.Sprintf("  %s --[%s]--> %s\n", from.String(), rpc, to.String())
		}
	}
	return result
}
