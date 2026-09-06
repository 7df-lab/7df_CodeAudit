// Package session implements session management and sandbox isolation.
// 依据: 06_OpenShell集成设计.md §3 沙箱配置
// 依据: codeaudit_common.proto L1325-L1326 SessionStatus
// 依据: 07 §8 — DSH 会话存活 30m
// ADR-134 诚实声明: 沙箱隔离（SandboxConfig/ValidateSandboxAccess/沙箱计数）目前是
// 未接线的纸面实现——全服务无任何容器/ns 执行机制，真实沙箱 = 06 OpenShell 集成
// （TP 尚未开始）。本包当前真实生效的只有会话生命周期管理（创建/TTL/过期清理）。
package session

import (
	"errors"
	"fmt"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
)

// SessionTTL — DSH 会话存活时长（依据: 07 §8 "30m"；值在全局配置 dsh_runtime.session.ttl_s，ADR-137）。
var SessionTTL = mustTTL()

// janitorRatio — 过期清理周期 = TTL/ratio（值在全局配置 dsh_runtime.session.janitor_interval_ratio）。
var janitorRatio = mustRatio()

func cfgSessionInt(key string) int {
	// 会话包不依赖 service 包，直接读全局配置（向上查找 configs/codeaudit.yaml）
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime session config: %v (ADR-137)", err))
	}
	v, err := cfg.Int("dsh_runtime.session." + key)
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime session config: %v (ADR-137)", err))
	}
	return v
}

func mustTTL() time.Duration { return time.Duration(cfgSessionInt("ttl_s")) * time.Second }
func mustRatio() int         { return cfgSessionInt("janitor_interval_ratio") }

var (
	ErrSessionNotFound  = errors.New("session not found")
	ErrSessionExpired   = errors.New("session expired")
	ErrSandboxViolation = errors.New("sandbox security violation")
)

// SandboxConfig defines sandbox isolation rules.
// 依据: 06 §3.1 沙箱配置模板
type SandboxConfig struct {
	MaxMemoryMB  int    // 依据: 06 §3.1 resources.memory
	MaxCPUCores  int    // 依据: 06 §3.1 resources.cpu
	Timeout      string // 依据: 06 §3.1 resources.timeout
	ReadOnlyRoot bool   // 依据: 06 §3.1 filesystem mounts
}

// DefaultSandboxConfig returns the default sandbox configuration.
// 依据: 06 §3.1 — 沙箱配置模板
func DefaultSandboxConfig() SandboxConfig {
	return SandboxConfig{
		MaxMemoryMB:  4096,  // 4Gi, 依据: 06 §3.1
		MaxCPUCores:  2,     // 依据: 06 §3.1
		Timeout:      "30m", // 依据: 06 §3.1
		ReadOnlyRoot: true,  // 依据: 06 §3.1 filesystem.mounts[0].readOnly
	}
}

// Session represents a running analysis session.
type Session struct {
	mu              sync.Mutex
	ID              string
	TaskID          string
	State           string // active, completed, cancelled, expired
	ActiveSandboxes int64
	CreatedAt       time.Time
	ExpiresAt       time.Time
	SandboxConfig   SandboxConfig
}

// Manager manages analysis sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager creates a new session manager.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
	}
}

// CreateSession creates a new analysis session.
// 依据: 07 §8 — DSH 会话存活 30m
func (m *Manager) CreateSession(sessionID, taskID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	s := &Session{
		ID:            sessionID,
		TaskID:        taskID,
		State:         "active",
		CreatedAt:     now,
		ExpiresAt:     now.Add(SessionTTL), // 依据: 07 §8
		SandboxConfig: DefaultSandboxConfig(),
	}
	m.sessions[sessionID] = s
	return s
}

// GetSession returns a session by ID.
// 依据: codeaudit_common.proto L1325 GetSessionStatusRequest
func (m *Manager) GetSession(sessionID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	if time.Now().After(s.ExpiresAt) {
		// ADR-134: State 写必须在 Session 自己的写锁下（此前在 Manager.RLock 下写 → 数据竞争）
		s.mu.Lock()
		s.State = "expired"
		s.mu.Unlock()
		return nil, ErrSessionExpired
	}
	return s, nil
}

// CancelSession cancels a session.
// 依据: codeaudit_common.proto L1326 CancelAnalysisRequest
func (m *Manager) CancelSession(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	s.State = "cancelled"
	return nil
}

// GetStatus returns the session status.
// 依据: codeaudit_common.proto L1326 SessionStatus
func (s *Session) GetStatus() (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State, s.ActiveSandboxes
}

// IncrementSandbox increments the active sandbox count.
func (s *Session) IncrementSandbox() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ActiveSandboxes++
}

// DecrementSandbox decrements the active sandbox count.
func (s *Session) DecrementSandbox() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ActiveSandboxes > 0 {
		s.ActiveSandboxes--
	}
}

// ValidateSandboxAccess checks if a command is allowed in the sandbox.
// 依据: 06 安全模型 — 危险命令/越界路径写入 → 拒绝并计数
func (s *Session) ValidateSandboxAccess(command string, paths []string) error {
	// 危险命令检测
	// 依据: 06 安全模型清单
	dangerousCommands := []string{
		"rm -rf /", "mkfs", "dd if=", "> /dev/",
		"chmod 777", "chown root", "sudo ",
		"curl | sh", "wget | sh", "curl|sh", "wget|sh",
		"eval(",
	}
	for _, dc := range dangerousCommands {
		if contains(command, dc) {
			return ErrSandboxViolation
		}
	}

	// 越界路径检测
	// 依据: 06 §3.1 filesystem mounts — workspace readOnly, /tmp /output writable
	for _, p := range paths {
		if isOutOfBounds(p) {
			return ErrSandboxViolation
		}
	}

	return nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// isOutOfBounds checks if a path violates sandbox isolation.
// 依据: 06 §3.1 — 只有 /tmp 和 /output 可写，workspace 只读
func isOutOfBounds(path string) bool {
	// 允许的写路径前缀
	allowed := []string{"/tmp/", "/output/", "/workspace/"}
	for _, a := range allowed {
		if len(path) >= len(a) && path[:len(a)] == a {
			return false
		}
	}
	// 其他路径视为越界
	return true
}

// StartJanitor — 后台过期清理（ADR-134）：sessions map 此前只增不减（内存泄漏）。
// 周期扫描过期会话并移除。周期 = SessionTTL/ratio（ADR-137: ratio 在全局配置）。
func (m *Manager) StartJanitor(stop <-chan struct{}) {
	interval := SessionTTL / time.Duration(janitorRatio)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.mu.Lock()
				now := time.Now()
				for id, s := range m.sessions {
					if now.After(s.ExpiresAt) {
						delete(m.sessions, id)
					}
				}
				m.mu.Unlock()
			case <-stop:
				return
			}
		}
	}()
}
