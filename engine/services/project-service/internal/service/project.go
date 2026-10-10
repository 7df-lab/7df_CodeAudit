// Package service implements business logic for the Project domain.
package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProjectService holds business methods for project operations.
type ProjectService struct {
	store  *repo.MemoryStore
	mirror *repo.PGMirror // HA P1-a（ADR-250）：nil=内存档（现状）；非 nil=写穿+读回落
}

// NewProjectService creates a ProjectService backed by the given store.
func NewProjectService(store *repo.MemoryStore) *ProjectService {
	return &ProjectService{store: store}
}

// SetMirror — 装配期注入 PG 镜像（main.go：DSN 非空时）。
func (s *ProjectService) SetMirror(m *repo.PGMirror) { s.mirror = m }

// Create creates a new project, assigning a project_id and created_at if missing.
// Returns a new Project without mutating the input (idempotency safety, 03 §2).
func (s *ProjectService) Create(p *v1.Project) *v1.Project {
	out := &v1.Project{
		ProjectId:       p.GetProjectId(),
		Name:            p.GetName(),
		RepoUrl:         p.GetRepoUrl(),
		DefaultBranch:   p.GetDefaultBranch(),
		DefaultScanMode: p.GetDefaultScanMode(),
		CreatedAt:       p.GetCreatedAt(),
		Team:            p.GetTeam(), // ADR-247: team 归属声明（workspace 恒等映射源）
	}
	if out.GetProjectId() == "" {
		out.ProjectId = "proj-" + generateID()
	}
	if out.GetCreatedAt() == nil {
		out.CreatedAt = timestamppb.New(time.Now())
	}
	s.store.CreateProject(&repo.ProjectRecord{Project: out})

	// Create default config and stats for the new project
	s.store.UpsertProjectConfig(&repo.ProjectConfigRecord{
		Config: &v1.ProjectConfig{
			ProjectId: out.GetProjectId(),
			Config:    map[string]string{},
		},
	})
	s.store.UpsertProjectStats(&repo.ProjectStatsRecord{
		Stats: &v1.ProjectStats{
			ProjectId: out.GetProjectId(),
		},
	})
	if s.mirror != nil { // HA P1-a 写穿（best-effort，ADR-250）
		if err := s.mirror.UpsertProject(&repo.ProjectRecord{Project: out}); err != nil {
			mirrorErr("project", out.GetProjectId(), err)
		}
		if err := s.mirror.UpsertProjectConfig(&repo.ProjectConfigRecord{Config: &v1.ProjectConfig{
			ProjectId: out.GetProjectId(), Config: map[string]string{}}}); err != nil {
			mirrorErr("project-config", out.GetProjectId(), err)
		}
		if err := s.mirror.UpsertProjectStats(&repo.ProjectStatsRecord{Stats: &v1.ProjectStats{
			ProjectId: out.GetProjectId()}}); err != nil {
			mirrorErr("project-stats", out.GetProjectId(), err)
		}
	}

	return out
}

// Get returns a project by ID.
func (s *ProjectService) Get(projectID string) (*v1.Project, bool) {
	rec, ok := s.store.GetProject(projectID)
	if !ok && s.mirror != nil { // HA P1-a 读回落（跨副本可见面）
		var err error
		rec, ok, err = s.mirror.GetProject(projectID)
		if err != nil {
			mirrorErr("project-fallback", projectID, err)
			ok = false
		} else if ok {
			s.store.CreateProject(rec)
		}
	}
	if !ok {
		return nil, false
	}
	return rec.Project, true
}

// Update replaces the stored project; requires the project to exist.
func (s *ProjectService) Update(p *v1.Project) (*v1.Project, bool) {
	_, ok := s.Get(p.GetProjectId()) // HA P1-a：前置存在性走回落版（跨副本 Update 语义）
	if !ok {
		return nil, false
	}
	s.store.UpdateProject(&repo.ProjectRecord{Project: p})
	if s.mirror != nil {
		if err := s.mirror.UpsertProject(&repo.ProjectRecord{Project: p}); err != nil {
			mirrorErr("project", p.GetProjectId(), err)
		}
	}
	return p, true
}

// Delete removes a project by ID.
func (s *ProjectService) Delete(projectID string) bool {
	_, ok := s.Get(projectID) // HA P1-a：前置存在性走回落版
	if !ok {
		return false
	}
	s.store.DeleteProject(projectID)
	if s.mirror != nil {
		if err := s.mirror.DeleteProject(projectID); err != nil {
			mirrorErr("project-delete", projectID, err)
		}
	}
	return true
}

// List returns all projects (simple implementation; pagination handled at handler level).
func (s *ProjectService) List() []*v1.Project {
	if s.mirror != nil { // HA P1-a：List 权威面——PG 全量同步补缺失再走内存排序
		s.syncProjectsFromPG(s.mirror)
	}
	recs := s.store.ListProjects()
	out := make([]*v1.Project, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Project)
	}
	return out
}

// GetConfig returns the config for a project.
func (s *ProjectService) GetConfig(projectID string) (*v1.ProjectConfig, bool) {
	rec, ok := s.store.GetProjectConfig(projectID)
	if !ok && s.mirror != nil {
		var err error
		rec, ok, err = s.mirror.GetProjectConfig(projectID)
		if err != nil {
			mirrorErr("config-fallback", projectID, err)
			ok = false
		} else if ok {
			s.store.UpsertProjectConfig(rec)
		}
	}
	if !ok {
		return nil, false
	}
	return rec.Config, true
}

// UpdateConfig upserts the config for a project.
func (s *ProjectService) UpdateConfig(cfg *v1.ProjectConfig) *v1.ProjectConfig {
	s.store.UpsertProjectConfig(&repo.ProjectConfigRecord{Config: cfg})
	if s.mirror != nil {
		if err := s.mirror.UpsertProjectConfig(&repo.ProjectConfigRecord{Config: cfg}); err != nil {
			mirrorErr("project-config", cfg.GetProjectId(), err)
		}
	}
	return cfg
}

// GetStats returns project statistics.
func (s *ProjectService) GetStats(projectID string) (*v1.ProjectStats, bool) {
	rec, ok := s.store.GetProjectStats(projectID)
	if !ok && s.mirror != nil {
		var err error
		rec, ok, err = s.mirror.GetProjectStats(projectID)
		if err != nil {
			mirrorErr("stats-fallback", projectID, err)
			ok = false
		} else if ok {
			s.store.UpsertProjectStats(rec)
		}
	}
	if !ok {
		return nil, false
	}
	return rec.Stats, true
}

// AddMember adds a member to a project.
func (s *ProjectService) AddMember(m *v1.ProjectMember) *v1.ProjectMember {
	s.store.AddMember(&repo.ProjectMemberRecord{Member: m})
	if s.mirror != nil {
		if err := s.mirror.UpsertMember(&repo.ProjectMemberRecord{Member: m}); err != nil {
			mirrorErr("member", m.GetProjectId()+":"+m.GetUserId(), err)
		}
	}
	return m
}

// RemoveMember removes a member from a project.
func (s *ProjectService) RemoveMember(projectID, userID string) bool {
	// We don't track existence separately; just delete
	s.store.RemoveMember(projectID, userID)
	if s.mirror != nil {
		if err := s.mirror.DeleteMember(projectID, userID); err != nil {
			mirrorErr("member-delete", projectID+":"+userID, err)
		}
	}
	return true
}

// ListMembers returns all members for a project.
func (s *ProjectService) ListMembers(projectID string) []*v1.ProjectMember {
	if s.mirror != nil { // 权威面：PG 拉全量补缺失（成员表无排序语义，合流安全）
		recs, err := s.mirror.ListMembers(projectID)
		if err != nil {
			mirrorErr("members-sync", projectID, err)
		} else {
			for _, rec := range recs {
				s.store.AddMember(rec)
			}
		}
	}
	recs := s.store.ListMembers(projectID)
	out := make([]*v1.ProjectMember, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Member)
	}
	return out
}

// generateID returns a short random hex string for IDs.
// R65：改 crypto/rand 8 字节——原 UnixNano&0xFFFFFFFF 截断
// 32 位相差恰 4.295s 的两次生成必然同 ID，且时间戳形态可预测（proj-/user- 共用）。
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err)) // 系统级熵源失败=不可继续
	}
	return hex.EncodeToString(b)
}
