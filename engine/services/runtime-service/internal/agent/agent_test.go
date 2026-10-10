package agent

import (
	"testing"
)

// 依据: 05 §4 五角色推理流程
// 依据: 07 §8.1 Agent 迭代上限

func TestNewAgentValidTypes(t *testing.T) {
	types := []AgentType{
		AgentCodeAnalyst, AgentVulnDetector, AgentSeverityAssessor,
		AgentFixAdvisor, AgentQualityValidator,
	}
	for _, at := range types {
		a, err := NewAgent(at)
		if err != nil {
			t.Fatalf("failed to create agent %s: %v", at, err)
		}
		if a.Type != at {
			t.Errorf("expected type %s, got %s", at, a.Type)
		}
		if a.Status != AgentStatusIdle {
			t.Errorf("expected idle status, got %s", a.Status)
		}
	}
}

func TestNewAgentInvalidType(t *testing.T) {
	_, err := NewAgent("invalid_agent")
	if err == nil {
		t.Fatal("expected error for invalid agent type")
	}
}

func TestAgentIterateWithinLimit(t *testing.T) {
	a, _ := NewAgent(AgentCodeAnalyst) // max 50 iterations
	for i := 0; i < 50; i++ {
		if err := a.Iterate(); err != nil {
			t.Fatalf("iteration %d should succeed: %v", i+1, err)
		}
	}
	if a.GetIteration() != 50 {
		t.Errorf("expected 50 iterations, got %d", a.GetIteration())
	}
}

func TestAgentIterateExceedsLimit(t *testing.T) {
	// 依据: 07 §8.1 — severity_assessor max 10 iterations
	a, _ := NewAgent(AgentSeverityAssessor)
	for i := 0; i < 10; i++ {
		a.Iterate()
	}
	// 第 11 次迭代应返回错误
	err := a.Iterate()
	if err != ErrMaxIterationsExceeded {
		t.Fatalf("expected ErrMaxIterationsExceeded, got %v", err)
	}
	if a.GetStatus() != AgentStatusTimeout {
		t.Errorf("expected timeout status, got %s", a.GetStatus())
	}
}

// 反向测试: 依据 test-gates.md §3 "超时"行
// 超过 07 §8 矩阵值 → TIMEOUT 而非挂死
func TestAgentIterationLimitsMatchSpec(t *testing.T) {
	// 依据: 07 §8.1 — 五 Agent 迭代上限
	expected := map[AgentType]int32{
		AgentCodeAnalyst:      50,
		AgentVulnDetector:     30,
		AgentSeverityAssessor: 10,
		AgentFixAdvisor:       15,
		AgentQualityValidator: 10,
	}

	for agentType, maxIter := range expected {
		config, ok := AgentConfigs[agentType]
		if !ok {
			t.Errorf("missing config for %s", agentType)
			continue
		}
		if config.MaxIterations != maxIter {
			t.Errorf("%s: expected max_iterations=%d, got %d",
				agentType, maxIter, config.MaxIterations)
		}
	}
}

func TestAgentStatusTransitions(t *testing.T) {
	a, _ := NewAgent(AgentVulnDetector)

	if a.GetStatus() != AgentStatusIdle {
		t.Fatal("expected initial idle status")
	}

	a.SetStatus(AgentStatusRunning)
	if a.GetStatus() != AgentStatusRunning {
		t.Fatal("expected running status")
	}

	a.SetStatus(AgentStatusCompleted)
	if a.GetStatus() != AgentStatusCompleted {
		t.Fatal("expected completed status")
	}
}
