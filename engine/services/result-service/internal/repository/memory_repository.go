// Package repository — 内存仓储实现。
// 用途：无 PostgreSQL 依赖的端到端验证路径（07 §10 降级策略精神：外部依赖不可用时功能不中断）；
// 数据库隔离权威口径见 09 §3（正式部署仍用 PostgresFindingRepository）。
package repository

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codeaudit/services/result-service/internal/model"
)

// MemoryFindingRepository 是 FindingRepository 的线程安全内存实现。
type MemoryFindingRepository struct {
	mu        sync.RWMutex
	findings  map[string]*model.Finding
	feedbacks map[string]*model.FindingFeedback
	order     []string // 插入顺序，稳定分页游标
}

func NewMemoryFindingRepository() *MemoryFindingRepository {
	return &MemoryFindingRepository{
		findings:  map[string]*model.Finding{},
		feedbacks: map[string]*model.FindingFeedback{},
	}
}

func (m *MemoryFindingRepository) Create(f *model.Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.findings[f.ID]; ok {
		return fmt.Errorf("finding %s already exists", f.ID)
	}
	cp := *f
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	cp.UpdatedAt = time.Now()
	m.findings[f.ID] = &cp
	m.order = append(m.order, f.ID)
	return nil
}

func (m *MemoryFindingRepository) GetByID(id string) (*model.Finding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.findings[id]
	if !ok {
		return nil, fmt.Errorf("finding %s not found", id)
	}
	cp := *f
	return &cp, nil
}

func (m *MemoryFindingRepository) Update(f *model.Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.findings[f.ID]; !ok {
		return fmt.Errorf("finding %s not found", f.ID)
	}
	cp := *f
	cp.UpdatedAt = time.Now()
	m.findings[f.ID] = &cp
	return nil
}

func (m *MemoryFindingRepository) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.findings[id]; !ok {
		return fmt.Errorf("finding %s not found", id)
	}
	delete(m.findings, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// List 复刻 03 §5 cursor 语义：lastID 之后 limit 条（按插入序）。
func (m *MemoryFindingRepository) List(lastID string, limit int, taskID string, verdict string) ([]*model.Finding, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	idx := 0
	if lastID != "" {
		for i, v := range m.order {
			if v == lastID {
				idx = i + 1
				break
			}
		}
	}
	out := []*model.Finding{}
	next := ""
	for ; idx < len(m.order); idx++ {
		f := m.findings[m.order[idx]]
		if taskID != "" && f.TaskID != taskID {
			continue
		}
		if verdict != "" && !strings.EqualFold(f.Verdict, verdict) {
			continue
		}
		out = append(out, func() *model.Finding { c := *f; return &c }())
		if len(out) >= limit {
			next = f.ID
			break
		}
	}
	return out, next, nil
}

func (m *MemoryFindingRepository) ListByVerdict(verdict string, lastID string, limit int) ([]*model.Finding, string, error) {
	return m.List(lastID, limit, "", verdict)
}

func (m *MemoryFindingRepository) GetByRequestIDAndFindingID(requestID string, findingID string) (*model.Finding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		f := m.findings[id]
		if f.RequestID == requestID && f.ID == findingID {
			c := *f
			return &c, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *MemoryFindingRepository) GetStatsByTaskID(taskID string) (*model.ResultStats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats := &model.ResultStats{
		TaskId:     taskID,
		BySeverity: map[string]int32{},
		ByCwe:      map[string]int32{},
		ByVerdict:  map[string]int32{},
	}
	for _, id := range m.order {
		f := m.findings[id]
		if f.TaskID != taskID {
			continue
		}
		stats.TotalFindings++
		stats.BySeverity[strings.ToUpper(f.Severity)]++
		stats.ByVerdict[f.Verdict]++
		// RuleID 承载 CWE 口径（模型中以 RuleID 存储）
		stats.ByCwe[f.RuleID]++
	}
	return stats, nil
}

func (m *MemoryFindingRepository) CreateFeedback(fb *model.FindingFeedback) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feedbacks[fb.ID] = fb
	return nil
}

func (m *MemoryFindingRepository) GetFeedbackByRequestID(requestID string) (*model.FindingFeedback, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, fb := range m.feedbacks {
		if fb.RequestID == requestID {
			c := *fb
			return &c, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

// DB() — 内存实现无 DB；返回 nil 仅用于满足 FindingRepository 接口（09 §3 正式部署走 Postgres）。
func (m *MemoryFindingRepository) DB() *sql.DB { return nil }
