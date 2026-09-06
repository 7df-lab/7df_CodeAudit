package saga

import (
	"errors"
	"sync"
	"testing"
)

// TestSaga_SuccessfulExecution tests that all steps execute successfully.
func TestSaga_SuccessfulExecution(t *testing.T) {
	saga := New("task-1")

	executed := make([]string, 0)
	var mu sync.Mutex

	// Add 3 successful steps
	for i := 0; i < 3; i++ {
		stepName := "step-" + string(rune('a'+i))
		saga.AddStep(stepName,
			func(taskID string, params map[string]interface{}) error {
				mu.Lock()
				defer mu.Unlock()
				executed = append(executed, stepName)
				return nil
			},
			nil, // No compensation needed
		)
	}

	failedIndex, err := saga.Execute(nil)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	if failedIndex != -1 {
		t.Errorf("failedIndex = %d, want -1", failedIndex)
	}

	// All steps should be executed
	if len(executed) != 3 {
		t.Errorf("executed count = %d, want 3", len(executed))
	}

	if !saga.IsCompleted() {
		t.Error("IsCompleted() = false, want true")
	}
}

// TestSaga_FailureTriggersCompensation tests that failure triggers compensation in reverse order.
func TestSaga_FailureTriggersCompensation(t *testing.T) {
	saga := New("task-1")

	compensated := make([]string, 0)
	var mu sync.Mutex

	// Add 3 steps, step 2 will fail
	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			compensated = append(compensated, "step-a")
			return nil
		},
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			compensated = append(compensated, "step-b")
			return nil
		},
	)

	saga.AddStep("step-c",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("step c failed")
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			compensated = append(compensated, "step-c")
			return nil
		},
	)

	failedIndex, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}
	if failedIndex != 2 {
		t.Errorf("failedIndex = %d, want 2", failedIndex)
	}

	// Compensation should be in reverse order: step-c, step-b, step-a
	if len(compensated) != 3 {
		t.Fatalf("compensated count = %d, want 3", len(compensated))
	}
	if compensated[0] != "step-c" {
		t.Errorf("compensated[0] = %q, want step-c", compensated[0])
	}
	if compensated[1] != "step-b" {
		t.Errorf("compensated[1] = %q, want step-b", compensated[1])
	}
	if compensated[2] != "step-a" {
		t.Errorf("compensated[2] = %q, want step-a", compensated[2])
	}

	if saga.IsCompleted() {
		t.Error("IsCompleted() = true, want false")
	}
}

// TestSaga_CompensationRetries tests that compensation retries up to 3 times.
func TestSaga_CompensationRetries(t *testing.T) {
	saga := New("task-1")

	attemptCount := 0
	var mu sync.Mutex

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			attemptCount++
			if attemptCount < 3 {
				return errors.New("compensation failed")
			}
			return nil // Succeed on 3rd attempt
		},
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("step b failed")
		},
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
	)

	_, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}

	// Compensation for step-a should have been attempted 3 times
	if attemptCount != 3 {
		t.Errorf("attemptCount = %d, want 3", attemptCount)
	}

	// step-a should be compensated
	status := saga.GetStepStatus()
	if status[0].Status != StepStatusCompensated {
		t.Errorf("step-a status = %v, want COMPENSATED", status[0].Status)
	}
}

// TestSaga_CompensationDeadLetter tests that failed compensation after 3 retries is dead-lettered.
func TestSaga_CompensationDeadLetter(t *testing.T) {
	saga := New("task-1")

	attemptCount := 0
	var mu sync.Mutex

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			attemptCount++
			return errors.New("compensation always fails")
		},
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("step b failed")
		},
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
	)

	_, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}

	// Should have attempted 3 times
	if attemptCount != 3 {
		t.Errorf("attemptCount = %d, want 3", attemptCount)
	}

	// step-a should NOT be compensated (dead-lettered)
	status := saga.GetStepStatus()
	if status[0].Status == StepStatusCompensated {
		t.Error("step-a should not be COMPENSATED after 3 failed attempts")
	}
}

// TestSaga_NoCompensationAction tests that steps without compensation action are skipped.
func TestSaga_NoCompensationAction(t *testing.T) {
	saga := New("task-1")

	compensated := make([]string, 0)
	var mu sync.Mutex

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		nil, // No compensation
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		func(taskID string, params map[string]interface{}) error {
			mu.Lock()
			defer mu.Unlock()
			compensated = append(compensated, "step-b")
			return nil
		},
	)

	saga.AddStep("step-c",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("step c failed")
		},
		nil, // No compensation
	)

	_, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}

	// Only step-b should be compensated (step-a has no compensation, step-c is the failing step)
	if len(compensated) != 1 || compensated[0] != "step-b" {
		t.Errorf("compensated = %v, want [step-b]", compensated)
	}
}

// TestSaga_GetStepStatus tests GetStepStatus method.
func TestSaga_GetStepStatus(t *testing.T) {
	saga := New("task-1")

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return nil
		},
		nil,
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("failed")
		},
		nil,
	)

	saga.Execute(nil)

	status := saga.GetStepStatus()
	if len(status) != 2 {
		t.Fatalf("status count = %d, want 2", len(status))
	}

	if status[0].Name != "step-a" || status[0].Status != StepStatusCompleted {
		t.Errorf("step-a: name=%s status=%v", status[0].Name, status[0].Status)
	}
	if status[1].Name != "step-b" || status[1].Status != StepStatusFailed {
		t.Errorf("step-b: name=%s status=%v", status[1].Name, status[1].Status)
	}
	if status[1].Error == nil {
		t.Error("step-b error should not be nil")
	}
}

// TestSaga_IsCompensated tests IsCompensated method.
func TestSaga_IsCompensated(t *testing.T) {
	tests := []struct {
		name     string
		steps    int
		failAt   int
		wantComp bool
	}{
		{
			name:     "all success",
			steps:    3,
			failAt:   -1,
			wantComp: true,
		},
		{
			name:     "failure with compensation",
			steps:    3,
			failAt:   2,
			wantComp: true, // All failed steps were compensated
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saga := New("task-1")

			for i := 0; i < tt.steps; i++ {
				stepName := "step-" + string(rune('a'+i))
				idx := i
				saga.AddStep(stepName,
					func(taskID string, params map[string]interface{}) error {
						if idx == tt.failAt {
							return errors.New("fail")
						}
						return nil
					},
					func(taskID string, params map[string]interface{}) error {
						return nil
					},
				)
			}

			saga.Execute(nil)

			if saga.IsCompensated() != tt.wantComp {
				t.Errorf("IsCompensated() = %v, want %v", saga.IsCompensated(), tt.wantComp)
			}
		})
	}
}

// TestSaga_ConcurrentAccess tests concurrent access to the saga.
func TestSaga_ConcurrentAccess(t *testing.T) {
	saga := New("task-1")

	for i := 0; i < 10; i++ {
		saga.AddStep("step-"+string(rune('a'+i)),
			func(taskID string, params map[string]interface{}) error {
				return nil
			},
			nil,
		)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			saga.GetStepStatus()
			saga.IsCompleted()
			saga.IsCompensated()
		}()
	}
	wg.Wait()
}

// TestSaga_StepStatusString tests StepStatus.String() method.
func TestSaga_StepStatusString(t *testing.T) {
	tests := []struct {
		status StepStatus
		want   string
	}{
		{StepStatusPending, "PENDING"},
		{StepStatusRunning, "RUNNING"},
		{StepStatusCompleted, "COMPLETED"},
		{StepStatusFailed, "FAILED"},
		{StepStatusCompensated, "COMPENSATED"},
		{StepStatus(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.status.String(); got != tt.want {
				t.Errorf("StepStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

// TestSaga_FirstStepFails tests that if the first step fails, compensation runs correctly.
func TestSaga_FirstStepFails(t *testing.T) {
	saga := New("task-1")

	compensated := false

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			return errors.New("first step fails")
		},
		func(taskID string, params map[string]interface{}) error {
			compensated = true
			return nil
		},
	)

	saga.AddStep("step-b",
		func(taskID string, params map[string]interface{}) error {
			t.Fatal("step-b should not execute")
			return nil
		},
		nil,
	)

	failedIndex, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}
	if failedIndex != 0 {
		t.Errorf("failedIndex = %d, want 0", failedIndex)
	}

	if !compensated {
		t.Error("step-a compensation should have been called")
	}
}

// TestSaga_LastStepFails tests that if the last step fails, all steps are compensated.
func TestSaga_LastStepFails(t *testing.T) {
	saga := New("task-1")

	compensated := make([]string, 0)
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		stepName := "step-" + string(rune('a'+i))
		idx := i
		saga.AddStep(stepName,
			func(taskID string, params map[string]interface{}) error {
				if idx == 4 {
					return errors.New("last step fails")
				}
				return nil
			},
			func(taskID string, params map[string]interface{}) error {
				mu.Lock()
				defer mu.Unlock()
				compensated = append(compensated, stepName)
				return nil
			},
		)
	}

	failedIndex, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}
	if failedIndex != 4 {
		t.Errorf("failedIndex = %d, want 4", failedIndex)
	}

	// All 5 steps should be compensated (in reverse order)
	if len(compensated) != 5 {
		t.Fatalf("compensated count = %d, want 5", len(compensated))
	}

	// Check reverse order
	expectedOrder := []string{"step-e", "step-d", "step-c", "step-b", "step-a"}
	for i, name := range expectedOrder {
		if compensated[i] != name {
			t.Errorf("compensated[%d] = %q, want %q", i, compensated[i], name)
		}
	}
}

// TestSaga_MiddleStepFails tests that if a middle step fails, only executed steps are compensated.
func TestSaga_MiddleStepFails(t *testing.T) {
	saga := New("task-1")

	compensated := make([]string, 0)
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		stepName := "step-" + string(rune('a'+i))
		idx := i
		saga.AddStep(stepName,
			func(taskID string, params map[string]interface{}) error {
				if idx == 2 {
					return errors.New("middle step fails")
				}
				return nil
			},
			func(taskID string, params map[string]interface{}) error {
				mu.Lock()
				defer mu.Unlock()
				compensated = append(compensated, stepName)
				return nil
			},
		)
	}

	failedIndex, err := saga.Execute(nil)
	if err == nil {
		t.Fatal("Execute() should return error")
	}
	if failedIndex != 2 {
		t.Errorf("failedIndex = %d, want 2", failedIndex)
	}

	// Steps 0, 1, 2 should be compensated (in reverse order)
	if len(compensated) != 3 {
		t.Fatalf("compensated count = %d, want 3", len(compensated))
	}

	expectedOrder := []string{"step-c", "step-b", "step-a"}
	for i, name := range expectedOrder {
		if compensated[i] != name {
			t.Errorf("compensated[%d] = %q, want %q", i, compensated[i], name)
		}
	}
}

// TestSaga_ParametersPassedToActions tests that parameters are passed to actions.
func TestSaga_ParametersPassedToActions(t *testing.T) {
	saga := New("task-1")

	var receivedParams map[string]interface{}

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			receivedParams = params
			return nil
		},
		nil,
	)

	params := map[string]interface{}{
		"key1": "value1",
		"key2": 42,
	}

	saga.Execute(params)

	if receivedParams == nil {
		t.Fatal("receivedParams is nil")
	}
	if receivedParams["key1"] != "value1" {
		t.Errorf("key1 = %v, want value1", receivedParams["key1"])
	}
	if receivedParams["key2"] != 42 {
		t.Errorf("key2 = %v, want 42", receivedParams["key2"])
	}
}

// TestSaga_TaskIDPassedToActions tests that taskID is passed to actions.
func TestSaga_TaskIDPassedToActions(t *testing.T) {
	saga := New("task-123")

	var receivedTaskID string

	saga.AddStep("step-a",
		func(taskID string, params map[string]interface{}) error {
			receivedTaskID = taskID
			return nil
		},
		nil,
	)

	saga.Execute(nil)

	if receivedTaskID != "task-123" {
		t.Errorf("taskID = %q, want %q", receivedTaskID, "task-123")
	}
}
