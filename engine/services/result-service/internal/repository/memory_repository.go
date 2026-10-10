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
		return nil // R135：与 PG ON CONFLICT (id) DO NOTHING 对齐——跨纪元重报幂等 no-op
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
func (m *MemoryFindingRepository) List(lastID string, limit int, taskID string, direction string, tier string) ([]*model.Finding, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	idx := 0
	if lastID != "" {
		found := false
		for i, v := range m.order {
			if v == lastID {
				idx = i + 1
				found = true
				break
			}
		}
		// R138（第四轮审计 P3-3）：游标引用已删条目时不得归零重放（整页重复）——
		// PG 版 keyset 语义天然跳过，内存档对齐为诚实空页。
		if !found {
			return []*model.Finding{}, "", nil
		}
	}
	out := []*model.Finding{}
	next := ""
	// R138（P3-3）：limit+1 探测定 next（原"恰好填满 limit 即置 next"在尾页恰好
	// 剩 limit 条时产出幻影下一页——PG 版同款探测语义）。
	for ; idx < len(m.order) && len(out) <= limit; idx++ {
		f := m.findings[m.order[idx]]
		if taskID != "" && f.TaskID != taskID {
			continue
		}
		if direction != "" && !strings.EqualFold(f.VerdictDirection, direction) {
			continue
		}
		if tier != "" && !strings.EqualFold(f.VerdictTier, tier) {
			continue
		}
		out = append(out, func() *model.Finding { c := *f; return &c }())
	}
	if len(out) > limit {
		next = out[limit-1].ID
		out = out[:limit]
	}
	return out, next, nil
}

func (m *MemoryFindingRepository) ListByVerdict(direction string, tier string, lastID string, limit int) ([]*model.Finding, string, error) {
	return m.List(lastID, limit, "", direction, tier)
}

// Count — 三过滤轴与 List 同语义的行数（PG 档 Count 的内存对齐；
// 匹配语义同款 EqualFold，游标轴不参与计数）
func (m *MemoryFindingRepository) Count(taskID string, direction string, tier string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, id := range m.order {
		f := m.findings[id]
		if taskID != "" && f.TaskID != taskID {
			continue
		}
		if direction != "" && !strings.EqualFold(f.VerdictDirection, direction) {
			continue
		}
		if tier != "" && !strings.EqualFold(f.VerdictTier, tier) {
			continue
		}
		n++
	}
	return n, nil
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
		// 桶口径与 PG 版对齐：两轴组合键
		d := strings.ToUpper(f.VerdictDirection)
		t := strings.ToUpper(f.VerdictTier)
		switch {
		case strings.HasSuffix(d, "_TRUE") && strings.HasSuffix(t, "_DEFINITE"):
			stats.ByVerdict["TRUE_DEFINITE"]++
		case strings.HasSuffix(d, "_FALSE") && strings.HasSuffix(t, "_DEFINITE"):
			stats.ByVerdict["FALSE_DEFINITE"]++
		default:
			stats.ByVerdict["NEEDS_REVIEW"]++
		}
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
