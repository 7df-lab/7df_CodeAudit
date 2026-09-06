// Package service implements business logic for the Project domain.
package service

import (
	"fmt"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProjectService holds business methods for project operations.
type ProjectService struct {
	store *repo.MemoryStore
}

// NewProjectService creates a ProjectService backed by the given store.
func NewProjectService(store *repo.MemoryStore) *ProjectService {
	return &ProjectService{store: store}
}

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

	return out
}

// Get returns a project by ID.
func (s *ProjectService) Get(projectID string) (*v1.Project, bool) {
	rec, ok := s.store.GetProject(projectID)
	if !ok {
		return nil, false
	}
	return rec.Project, true
}

// Update replaces the stored project; requires the project to exist.
func (s *ProjectService) Update(p *v1.Project) (*v1.Project, bool) {
	_, ok := s.store.GetProject(p.GetProjectId())
	if !ok {
		return nil, false
	}
	s.store.UpdateProject(&repo.ProjectRecord{Project: p})
	return p, true
}

// Delete removes a project by ID.
func (s *ProjectService) Delete(projectID string) bool {
	_, ok := s.store.GetProject(projectID)
	if !ok {
		return false
	}
	s.store.DeleteProject(projectID)
	return true
}

// List returns all projects (simple implementation; pagination handled at handler level).
func (s *ProjectService) List() []*v1.Project {
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
	if !ok {
		return nil, false
	}
	return rec.Config, true
}

// UpdateConfig upserts the config for a project.
func (s *ProjectService) UpdateConfig(cfg *v1.ProjectConfig) *v1.ProjectConfig {
	s.store.UpsertProjectConfig(&repo.ProjectConfigRecord{Config: cfg})
	return cfg
}

// GetStats returns project statistics.
func (s *ProjectService) GetStats(projectID string) (*v1.ProjectStats, bool) {
	rec, ok := s.store.GetProjectStats(projectID)
	if !ok {
		return nil, false
	}
	return rec.Stats, true
}

// AddMember adds a member to a project.
func (s *ProjectService) AddMember(m *v1.ProjectMember) *v1.ProjectMember {
	s.store.AddMember(&repo.ProjectMemberRecord{Member: m})
	return m
}

// RemoveMember removes a member from a project.
func (s *ProjectService) RemoveMember(projectID, userID string) bool {
	// We don't track existence separately; just delete
	s.store.RemoveMember(projectID, userID)
	return true
}

// ListMembers returns all members for a project.
func (s *ProjectService) ListMembers(projectID string) []*v1.ProjectMember {
	recs := s.store.ListMembers(projectID)
	out := make([]*v1.ProjectMember, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Member)
	}
	return out
}

// generateID returns a short random hex string for IDs.
func generateID() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
}
