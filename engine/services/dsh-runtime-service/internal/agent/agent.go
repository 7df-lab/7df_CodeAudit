// Package agent implements the five analysis agents for dsh-runtime-service.
// 依据: 05_知识与推理设计.md §4 五角色推理流程
// 依据: 07_非功能指标基线.md §8.1 Agent 迭代上限
// 依据: 06_OpenShell集成设计.md §3 沙箱配置
package agent

import (
	"errors"
	"sync"
)

// AgentType represents the five agent roles.
// 依据: 05 §4 — Code Analyst / Vuln Detector / Severity Assessor / Fix Advisor / Quality Validator
type AgentType string

const (
	AgentCodeAnalyst      AgentType = "code_analyst"
	AgentVulnDetector     AgentType = "vuln_detector"
	AgentSeverityAssessor AgentType = "severity_assessor"
	AgentFixAdvisor       AgentType = "fix_advisor"
	AgentQualityValidator AgentType = "quality_validator"
)

// AgentConfig holds agent resource limits.
// 依据: 07 §8.1 — Agent 迭代上限表
type AgentConfig struct {
	Type           AgentType
	MaxIterations  int32  // 最大迭代次数
	SandboxTimeout string // 沙箱超时
}

// 依据: 07 §8.1 — 五 Agent 迭代上限
var AgentConfigs = map[AgentType]AgentConfig{
	AgentCodeAnalyst: {
		Type:           AgentCodeAnalyst,
		MaxIterations:  50,    // 依据: 07 §8.1
		SandboxTimeout: "30m", // 依据: 07 §8.1
	},
	AgentVulnDetector: {
		Type:           AgentVulnDetector,
		MaxIterations:  30,    // 依据: 07 §8.1
		SandboxTimeout: "20m", // 依据: 07 §8.1
	},
	AgentSeverityAssessor: {
		Type:           AgentSeverityAssessor,
		MaxIterations:  10,    // 依据: 07 §8.1
		SandboxTimeout: "10m", // 依据: 07 §8.1
	},
	AgentFixAdvisor: {
		Type:           AgentFixAdvisor,
		MaxIterations:  15,    // 依据: 07 §8.1
		SandboxTimeout: "15m", // 依据: 07 §8.1
	},
	AgentQualityValidator: {
		Type:           AgentQualityValidator,
		MaxIterations:  10,    // 依据: 07 §8.1
		SandboxTimeout: "10m", // 依据: 07 §8.1
	},
}

var (
	ErrMaxIterationsExceeded = errors.New("maximum iterations exceeded")
	ErrSandboxTimeout        = errors.New("sandbox execution timeout")
	ErrSandboxViolation      = errors.New("sandbox security violation")
)

// Agent represents a running analysis agent instance.
type Agent struct {
	mu               sync.Mutex
	Type             AgentType
	Config           AgentConfig
	CurrentIteration int32
	Status           AgentStatus
}

// AgentStatus represents the current status of an agent.
type AgentStatus string

const (
	AgentStatusIdle      AgentStatus = "idle"
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusFailed    AgentStatus = "failed"
	AgentStatusTimeout   AgentStatus = "timeout"
)

// NewAgent creates a new agent instance.
func NewAgent(agentType AgentType) (*Agent, error) {
	config, ok := AgentConfigs[agentType]
	if !ok {
		return nil, errors.New("unknown agent type: " + string(agentType))
	}
	return &Agent{
		Type:   agentType,
		Config: config,
		Status: AgentStatusIdle,
	}, nil
}

// Iterate increments the iteration counter and checks the limit.
// 依据: 07 §8.1 — 超过迭代上限→中断返回部分结果
func (a *Agent) Iterate() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.CurrentIteration++
	if a.CurrentIteration > a.Config.MaxIterations {
		a.Status = AgentStatusTimeout
		return ErrMaxIterationsExceeded
	}
	return nil
}

// GetIteration returns the current iteration count.
func (a *Agent) GetIteration() int32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CurrentIteration
}

// SetStatus sets the agent status.
func (a *Agent) SetStatus(status AgentStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = status
}

// GetStatus returns the current agent status.
func (a *Agent) GetStatus() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status
}
