package session

import (
	"testing"
	"time"
)

// 依据: 06 §3 沙箱配置
// 依据: 07 §8 会话存活 30m

func TestCreateSession(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")
	if s.ID != "sess-1" {
		t.Errorf("expected sess-1, got %s", s.ID)
	}
	if s.State != "active" {
		t.Errorf("expected active, got %s", s.State)
	}
	// 依据: 07 §8 — 会话存活 30m
	if s.ExpiresAt.Sub(s.CreatedAt) != SessionTTL {
		t.Errorf("expected TTL %v, got %v", SessionTTL, s.ExpiresAt.Sub(s.CreatedAt))
	}
}

func TestGetSessionNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.GetSession("nonexistent")
	if err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestGetSessionExpired(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")
	// 强制过期
	s.ExpiresAt = time.Now().Add(-1 * time.Minute)

	_, err := m.GetSession("sess-1")
	if err != ErrSessionExpired {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}
}

func TestCancelSession(t *testing.T) {
	m := NewManager()
	m.CreateSession("sess-1", "task-1")

	err := m.CancelSession("sess-1")
	if err != nil {
		t.Fatal(err)
	}

	s, _ := m.GetSession("sess-1")
	if s.State != "cancelled" {
		t.Errorf("expected cancelled, got %s", s.State)
	}
}

func TestSessionSandboxCounters(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")

	s.IncrementSandbox()
	s.IncrementSandbox()
	state, active := s.GetStatus()
	if state != "active" || active != 2 {
		t.Errorf("expected active/2, got %s/%d", state, active)
	}

	s.DecrementSandbox()
	_, active = s.GetStatus()
	if active != 1 {
		t.Errorf("expected 1 sandbox, got %d", active)
	}
}

// 反向测试: 依据 test-gates.md §3 "沙箱安全"行
// 危险命令/越界路径写入 → 拒绝并计数
func TestSandboxDangerousCommand(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")

	dangerous := []string{
		"rm -rf /",
		"sudo rm -rf /var",
		"curl|sh",
		"chmod 777 /etc/passwd",
	}
	for _, cmd := range dangerous {
		err := s.ValidateSandboxAccess(cmd, nil)
		if err != ErrSandboxViolation {
			t.Errorf("expected sandbox violation for command: %s, got %v", cmd, err)
		}
	}
}

func TestSandboxSafeCommand(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")

	safe := []string{
		"ls -la /workspace",
		"cat /workspace/main.go",
		"go test ./...",
	}
	for _, cmd := range safe {
		err := s.ValidateSandboxAccess(cmd, nil)
		if err != nil {
			t.Errorf("command should be allowed: %s, got %v", cmd, err)
		}
	}
}

func TestSandboxPathValidation(t *testing.T) {
	m := NewManager()
	s := m.CreateSession("sess-1", "task-1")

	// 允许的路径
	err := s.ValidateSandboxAccess("touch", []string{"/tmp/test.txt", "/output/result.json"})
	if err != nil {
		t.Errorf("allowed paths should pass: %v", err)
	}

	// 越界的路径
	err = s.ValidateSandboxAccess("touch", []string{"/etc/passwd"})
	if err != ErrSandboxViolation {
		t.Errorf("out-of-bounds path should be rejected, got %v", err)
	}
}

func TestDefaultSandboxConfig(t *testing.T) {
	cfg := DefaultSandboxConfig()
	// 沙箱配置依据: 06 §3.1；会话存活依据: 07 §8
	if cfg.MaxMemoryMB != 4096 { // 依据: 06 §3.1
		t.Errorf("expected 4096MB, got %d", cfg.MaxMemoryMB)
	}
	if cfg.MaxCPUCores != 2 { // 依据: 06 §3.1
		t.Errorf("expected 2 cores, got %d", cfg.MaxCPUCores)
	}
	if cfg.Timeout != "30m" { // 依据: 07 §8
		t.Errorf("expected 30m, got %s", cfg.Timeout)
	}
	if !cfg.ReadOnlyRoot {
		t.Error("expected ReadOnlyRoot=true")
	}
}
