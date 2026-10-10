// Package repository — 内存报告仓储（无 PG 环境的 E2E 路径）。
package repository

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeaudit/services/result-service/internal/model"
)

// MemoryReportRepository 是 ReportRepository 的内存实现。
type MemoryReportRepository struct {
	mu      sync.RWMutex
	reports map[string]*model.Report
	order   []string
}

func NewMemoryReportRepository() *MemoryReportRepository {
	return &MemoryReportRepository{reports: map[string]*model.Report{}}
}

// DeleteReport — ADR-212: 同 DeleteReport 接口注释。
func (m *MemoryReportRepository) DeleteReport(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reports, id)
	return nil
}

func (m *MemoryReportRepository) CreateReport(r *model.Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reports[r.ID]; ok {
		return fmt.Errorf("report %s already exists", r.ID)
	}
	cp := *r
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	m.reports[r.ID] = &cp
	m.order = append(m.order, r.ID)
	return nil
}

// UpdateReport — ADR-199: URL 回写（内存档同语义）。
func (m *MemoryReportRepository) UpdateReport(r *model.Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reports[r.ID]; !ok {
		return fmt.Errorf("report %s not found", r.ID)
	}
	cp := *r
	m.reports[r.ID] = &cp
	return nil
}

func (m *MemoryReportRepository) GetReportByID(id string) (*model.Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.reports[id]
	if !ok {
		return nil, fmt.Errorf("report %s not found", id)
	}
	c := *r
	return &c, nil
}

func (m *MemoryReportRepository) GetReportByRequestID(requestID string) (*model.Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		r := m.reports[id]
		if r.RequestID == requestID {
			c := *r
			return &c, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *MemoryReportRepository) ListReports(q ListReportsQuery) ([]*model.Report, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// ADR-256：过滤（task_id/format）→排序（白名单键）→offset 切片；total=过滤后总数。
	filtered := make([]*model.Report, 0, len(m.order))
	for _, id := range m.order {
		r := m.reports[id]
		if q.TaskID != "" && !strings.EqualFold(r.TaskID, q.TaskID) {
			continue
		}
		if q.Format != "" && r.Format != q.Format {
			continue
		}
		filtered = append(filtered, r)
	}
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		// 键三分支（小于/大于/相等）各自定向；等键落 id 同向决胜——两分支写法
		// （less!=desc 提前返回）在 a>b 时误入决胜分支，非严格弱序出乱序（测试实证）。
		var aLess, bLess bool
		switch q.SortBy {
		case "task_id":
			aLess, bLess = a.TaskID < b.TaskID, b.TaskID < a.TaskID
		case "format":
			aLess, bLess = a.Format < b.Format, b.Format < a.Format
		case "generated_at":
			aLess, bLess = a.CreatedAt.Before(b.CreatedAt), b.CreatedAt.Before(a.CreatedAt)
		default: // report_id（缺省，旧口径）
			aLess, bLess = a.ID < b.ID, b.ID < a.ID
		}
		if aLess {
			return !q.SortDesc
		}
		if bLess {
			return q.SortDesc
		}
		return (a.ID < b.ID) != q.SortDesc // 等键决胜：id 同向，稳定序
	})
	total := len(filtered)
	offset := q.Offset
	if offset > total {
		offset = total
	}
	end := offset + q.Limit
	if end > total {
		end = total
	}
	return filtered[offset:end], total, nil
}

var builtinTemplates = []*model.ReportTemplate{
	{ID: "tpl-summary", Name: "融合审计摘要", Description: "融合报告模板：统计+误报+AI独有发现"},
	{ID: "tpl-comparison", Name: "对比报告", Description: "模式C四象限对比模板（04 §3.3）"},
	{ID: "tpl-review", Name: "审核报告", Description: "模式D整体评估+逐条审核模板（04 §3.4）"},
}

func (m *MemoryReportRepository) ListTemplates(limit int) ([]*model.ReportTemplate, error) {
	if limit <= 0 || limit > len(builtinTemplates) {
		limit = len(builtinTemplates)
	}
	return builtinTemplates[:limit], nil
}

func (m *MemoryReportRepository) GetTemplateByID(id string) (*model.ReportTemplate, error) {
	for _, t := range builtinTemplates {
		if t.ID == id {
			c := *t
			return &c, nil
		}
	}
	return nil, fmt.Errorf("template %s not found", id)
}
