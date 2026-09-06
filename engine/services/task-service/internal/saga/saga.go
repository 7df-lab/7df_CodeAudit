// Package saga provides Saga orchestration for task workflows.
// 依据: 04_工作流设计.md §2 Saga 编排 S1-S10
// 依据: .agent/research/brief-TP03-04-05.md §3.3 Saga S1-S10 完整表
package saga

import (
	"fmt"
	"log"
	"sync"
)

// StepStatus represents the status of a saga step.
type StepStatus int

const (
	StepStatusPending StepStatus = iota
	StepStatusRunning
	StepStatusCompleted
	StepStatusFailed
	StepStatusCompensated
)

func (s StepStatus) String() string {
	switch s {
	case StepStatusPending:
		return "PENDING"
	case StepStatusRunning:
		return "RUNNING"
	case StepStatusCompleted:
		return "COMPLETED"
	case StepStatusFailed:
		return "FAILED"
	case StepStatusCompensated:
		return "COMPENSATED"
	default:
		return "UNKNOWN"
	}
}

// StepAction is a function that executes a saga step.
type StepAction func(taskID string, params map[string]interface{}) error

// CompensatingAction is a function that compensates for a failed step.
type CompensatingAction func(taskID string, params map[string]interface{}) error

// SagaStep represents a single step in the saga.
type SagaStep struct {
	Name       string
	Action     StepAction
	Compensate CompensatingAction
	Status     StepStatus
	Error      error
	MaxRetries int
	RetryCount int
}

// Saga orchestrates a sequence of steps with compensation.
// 依据: 04 §2 Saga 编排
type Saga struct {
	mu     sync.Mutex
	steps  []*SagaStep
	taskID string
}

// New creates a new Saga instance.
func New(taskID string) *Saga {
	return &Saga{
		steps:  make([]*SagaStep, 0),
		taskID: taskID,
	}
}

// AddStep adds a step to the saga.
func (s *Saga) AddStep(name string, action StepAction, compensate CompensatingAction) {
	s.steps = append(s.steps, &SagaStep{
		Name:       name,
		Action:     action,
		Compensate: compensate,
		Status:     StepStatusPending,
		MaxRetries: 3, // 默认重试3次
	})
}

// Execute runs all saga steps in order. If any step fails, it compensates in reverse order.
// Returns the index of the failed step (-1 if all succeeded) and the error.
func (s *Saga) Execute(params map[string]interface{}) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[%s] Starting saga execution with %d steps", s.taskID, len(s.steps))

	// Execute steps forward
	for i, step := range s.steps {
		log.Printf("[%s] Executing step %d: %s", s.taskID, i, step.Name)
		step.Status = StepStatusRunning

		err := step.Action(s.taskID, params)
		if err != nil {
			step.Status = StepStatusFailed
			step.Error = err
			log.Printf("[%s] Step %d (%s) failed: %v", s.taskID, i, step.Name, err)

			// Compensate in reverse order
			s.compensate(i, params)

			return i, fmt.Errorf("saga step %d (%s) failed: %w", i, step.Name, err)
		}

		step.Status = StepStatusCompleted
		log.Printf("[%s] Step %d (%s) completed", s.taskID, i, step.Name)
	}

	log.Printf("[%s] Saga execution completed successfully", s.taskID)
	return -1, nil
}

// compensate runs compensating actions in reverse order for steps up to failedIndex.
func (s *Saga) compensate(failedIndex int, params map[string]interface{}) {
	log.Printf("[%s] Starting compensation for steps 0..%d", s.taskID, failedIndex)

	for i := failedIndex; i >= 0; i-- {
		step := s.steps[i]
		if step.Compensate == nil {
			log.Printf("[%s] Step %d (%s) has no compensating action, skipping", s.taskID, i, step.Name)
			continue
		}

		log.Printf("[%s] Compensating step %d: %s", s.taskID, i, step.Name)

		// Retry compensation up to 3 times before dead-letter
		// 依据: brief-TP03-04-05.md §3.3 "补偿失败重试3次后记 dead-letter"
		compensated := false
		for retry := 0; retry < 3; retry++ { // 依据: 07 §8 重试次数
			err := step.Compensate(s.taskID, params)
			if err == nil {
				compensated = true
				break
			}
			log.Printf("[%s] Compensation for step %d failed (attempt %d): %v",
				s.taskID, i, retry+1, err)
		}

		if compensated {
			step.Status = StepStatusCompensated
			log.Printf("[%s] Step %d (%s) compensated", s.taskID, i, step.Name)
		} else {
			// Dead-letter: compensation failed after 3 retries
			log.Printf("[%s] WARNING: Step %d (%s) compensation failed after 3 retries, dead-lettered",
				s.taskID, i, step.Name)
		}
	}
}

// GetStepStatus returns the status of all steps.
func (s *Saga) GetStepStatus() []struct {
	Name   string
	Status StepStatus
	Error  error
} {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]struct {
		Name   string
		Status StepStatus
		Error  error
	}, len(s.steps))

	for i, step := range s.steps {
		result[i].Name = step.Name
		result[i].Status = step.Status
		result[i].Error = step.Error
	}

	return result
}

// IsCompleted returns true if all steps completed successfully.
func (s *Saga) IsCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, step := range s.steps {
		if step.Status != StepStatusCompleted {
			return false
		}
	}
	return true
}

// IsCompensated returns true if all failed steps were compensated.
func (s *Saga) IsCompensated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, step := range s.steps {
		if step.Status == StepStatusFailed {
			return false
		}
	}
	return true
}

// 说明（ADR-131）: 原 BuildDefaultSaga S1-S10 各步 Action/Compensate 仅打日志即 return nil，
// 属"日志空壳冒充 Saga 实现"，且从未被生产路径调用（真实补偿已实装于 orchestrator.Execute
// 错误路径 / compensateFindings）。已删除。Saga 引擎（本文件其余部分）保留供编排器复用。
