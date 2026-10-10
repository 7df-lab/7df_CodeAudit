package service

// TestCreate_PreservesTeam — ADR-247（sim e2e 抓红实录）：Create 手写白名单映射
// 漏 Team → Project.team 恒空 → workspace 贯穿断链（task 快照回落缺省区）。
// 新 proto 字段加白名单拷贝时必须同步此处（同型回归锁定）。

import (
	"testing"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

func TestCreate_PreservesTeam(t *testing.T) {
	s := NewProjectService(repo.NewMemoryStore(false))
	out := s.Create(&v1.Project{Name: "p", Team: "alpha"})
	if out.GetTeam() != "alpha" {
		t.Fatalf("Create must preserve team (workspace 恒等映射源), got %q", out.GetTeam())
	}
	got, ok := s.Get(out.GetProjectId())
	if !ok || got.GetTeam() != "alpha" {
		t.Fatalf("stored project team: got %+v", got)
	}
}
