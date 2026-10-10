// ADR-256：内存档报告列表的服务端排序/筛选/offset+total 语义（sim 内存模式主路径）。
package repository

import (
	"testing"
	"time"

	"github.com/codeaudit/services/result-service/internal/model"
)

func seedReports() *MemoryReportRepository {
	m := NewMemoryReportRepository()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, spec := range []struct {
		id, task, format string
		hrs              int
	}{
		{"r-1", "t-a", "REPORT_FORMAT_JSON", 3},
		{"r-2", "t-b", "REPORT_FORMAT_HTML", 1},
		{"r-3", "t-a", "REPORT_FORMAT_JSON", 2},
		{"r-4", "t-b", "REPORT_FORMAT_JSON", 5},
	} {
		rep := &model.Report{ID: spec.id, TaskID: spec.task, Format: spec.format, CreatedAt: base.Add(time.Duration(spec.hrs) * time.Hour)}
		_ = i
		m.CreateReport(rep)
	}
	return m
}

func TestMemoryListReports_OrderByGeneratedAt(t *testing.T) {
	m := seedReports()
	rows, total, err := m.ListReports(ListReportsQuery{Limit: 10, SortBy: "generated_at"})
	if err != nil || total != 4 {
		t.Fatalf("err=%v total=%d", err, total)
	}
	want := []string{"r-2", "r-3", "r-1", "r-4"} // 1h,2h,3h,5h 升序
	for i, r := range rows {
		if r.ID != want[i] {
			t.Fatalf("asc[%d]=%s want %s (%v)", i, r.ID, want[i], rows)
		}
	}
	rows, _, _ = m.ListReports(ListReportsQuery{Limit: 10, SortBy: "generated_at", SortDesc: true})
	got := []string{}
	for _, r := range rows {
		got = append(got, r.ID)
	}
	if got[0] != "r-4" || got[3] != "r-2" {
		t.Fatalf("desc 序: %v", got)
	}
}

func TestMemoryListReports_FilterAndOffset(t *testing.T) {
	m := seedReports()
	rows, total, _ := m.ListReports(ListReportsQuery{Limit: 10, TaskID: "t-a"})
	if total != 2 || rows[0].ID != "r-1" {
		t.Fatalf("task 过滤: total=%d rows=%v", total, rows)
	}
	rows, total, _ = m.ListReports(ListReportsQuery{Limit: 10, Format: "REPORT_FORMAT_HTML"})
	if total != 1 || rows[0].ID != "r-2" {
		t.Fatalf("format 过滤: total=%d", total)
	}
	rows, total, _ = m.ListReports(ListReportsQuery{Limit: 2, SortBy: "report_id"})
	if total != 4 || len(rows) != 2 {
		t.Fatalf("offset 切片: total=%d len=%d", total, len(rows))
	}
	if rows[0].ID != "r-1" || rows[1].ID != "r-2" { // repo 层显式 ASC（DESC 缺省语义在 service 层注入）
		t.Fatalf("显式升序: %s,%s", rows[0].ID, rows[1].ID)
	}
	rows, _, _ = m.ListReports(ListReportsQuery{Limit: 2, SortBy: "report_id", SortDesc: true})
	if rows[0].ID != "r-4" || rows[1].ID != "r-3" {
		t.Fatalf("降序: %s,%s", rows[0].ID, rows[1].ID)
	}
}
