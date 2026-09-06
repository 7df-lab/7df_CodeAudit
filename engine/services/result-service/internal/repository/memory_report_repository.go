// Package repository — 内存报告仓储（无 PG 环境的 E2E 路径）。
package repository

import (
	"fmt"
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

func (m *MemoryReportRepository) ListReports(lastID string, limit int, taskID string) ([]*model.Report, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// ADR-142: 报告中心新→旧（插入序倒序）。游标 lastID = 上页最后一条 ID，
	// 从其插入位置之前（更旧）继续。
	start := len(m.order) - 1
	if lastID != "" {
		for i, v := range m.order {
			if v == lastID {
				start = i - 1
				break
			}
		}
	}
	out := []*model.Report{}
	next := ""
	for idx := start; idx >= 0; idx-- {
		r := m.reports[m.order[idx]]
		if taskID != "" && !strings.EqualFold(r.TaskID, taskID) {
			continue
		}
		out = append(out, func() *model.Report { c := *r; return &c }())
		if len(out) >= limit {
			next = r.ID
			break
		}
	}
	return out, next, nil
}

// 内置模板集（与 Postgres 实现的 report_templates 种子语义一致，proto L1268）
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
