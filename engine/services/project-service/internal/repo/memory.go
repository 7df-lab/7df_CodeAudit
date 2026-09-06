// Package repo implements an in-memory storage layer for projects and users.
// All data lives in Go maps protected by a sync.RWMutex.
package repo

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	v1 "github.com/codeaudit/proto-gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---- Project ----

type ProjectRecord struct {
	Project *v1.Project
}

// ---- ProjectConfig ----

type ProjectConfigRecord struct {
	Config *v1.ProjectConfig
}

// ---- ProjectStats ----

type ProjectStatsRecord struct {
	Stats *v1.ProjectStats
}

// ---- ProjectMember ----

type ProjectMemberRecord struct {
	Member *v1.ProjectMember
}

// ---- User ----

type UserRecord struct {
	User     *v1.User
	Password string // bcrypt hash（V2.1 ADR-205；不再存明文）
}

// MemoryStore is the single in-memory store for all entities.
type MemoryStore struct {
	mu sync.RWMutex

	// Projects: project_id → ProjectRecord
	projects map[string]*ProjectRecord
	// ProjectConfigs: project_id → ProjectConfigRecord
	projectConfigs map[string]*ProjectConfigRecord
	// ProjectStats: project_id → ProjectStatsRecord
	projectStats map[string]*ProjectStatsRecord
	// ProjectMembers: "project_id:user_id" → ProjectMemberRecord
	projectMembers map[string]*ProjectMemberRecord
	// Users: user_id → UserRecord
	users map[string]*UserRecord
}

// NewMemoryStore creates a MemoryStore pre-seeded with a demo user.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		projects:       make(map[string]*ProjectRecord),
		projectConfigs: make(map[string]*ProjectConfigRecord),
		projectStats:   make(map[string]*ProjectStatsRecord),
		projectMembers: make(map[string]*ProjectMemberRecord),
		users:          make(map[string]*UserRecord),
	}
	// Seed a default admin user for login testing.
	// V2.1 (ADR-205): 密码一律 bcrypt 存储（含种子账号，Login 侧 CompareHashAndPassword）；
	// role 显式 ROLE_ADMIN——JWT role claim 由此签发，网关 admin 门禁依赖。
	adminHash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("seed admin user: %v", err))
	}
	s.users["user-001"] = &UserRecord{
		User: &v1.User{
			UserId:    "user-001",
			Username:  "admin",
			Email:     "admin@codeaudit.local",
			State:     v1.User_USER_STATE_ACTIVE,
			Role:      v1.Role_ROLE_ADMIN,
			CreatedAt: timestamppb.New(time.Now()),
		},
		Password: string(adminHash),
	}
	return s
}

// ---- Project CRUD ----

func (m *MemoryStore) CreateProject(rec *ProjectRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projects[rec.Project.GetProjectId()] = rec
}

func (m *MemoryStore) GetProject(projectID string) (*ProjectRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.projects[projectID]
	return rec, ok
}

func (m *MemoryStore) UpdateProject(rec *ProjectRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projects[rec.Project.GetProjectId()] = rec
}

func (m *MemoryStore) DeleteProject(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projects, projectID)
	delete(m.projectConfigs, projectID)
	delete(m.projectStats, projectID)
	// remove all members for this project
	for key := range m.projectMembers {
		if len(key) > len(projectID) && key[:len(projectID)] == projectID && key[len(projectID)] == ':' {
			delete(m.projectMembers, key)
		}
	}
}

func (m *MemoryStore) ListProjects() []*ProjectRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*ProjectRecord, 0, len(m.projects))
	for _, rec := range m.projects {
		out = append(out, rec)
	}
	// ADR-135: 稳定排序（created_at 升序 + ID 决胜）——map 遍历序随机会导致
	// offset 分页重复/丢数据（03 §5 稳定游标语义）
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Project.GetCreatedAt().AsTime().Equal(out[j].Project.GetCreatedAt().AsTime()) {
			return out[i].Project.GetCreatedAt().AsTime().Before(out[j].Project.GetCreatedAt().AsTime())
		}
		return out[i].Project.GetProjectId() < out[j].Project.GetProjectId()
	})
	return out
}

// ---- ProjectConfig ----

func (m *MemoryStore) GetProjectConfig(projectID string) (*ProjectConfigRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.projectConfigs[projectID]
	return rec, ok
}

func (m *MemoryStore) UpsertProjectConfig(rec *ProjectConfigRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projectConfigs[rec.Config.GetProjectId()] = rec
}

// ---- ProjectStats ----

func (m *MemoryStore) GetProjectStats(projectID string) (*ProjectStatsRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.projectStats[projectID]
	return rec, ok
}

func (m *MemoryStore) UpsertProjectStats(rec *ProjectStatsRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projectStats[rec.Stats.GetProjectId()] = rec
}

// ---- ProjectMembers ----

func memberKey(projectID, userID string) string {
	return projectID + ":" + userID
}

func (m *MemoryStore) AddMember(rec *ProjectMemberRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projectMembers[memberKey(rec.Member.GetProjectId(), rec.Member.GetUserId())] = rec
}

func (m *MemoryStore) RemoveMember(projectID, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projectMembers, memberKey(projectID, userID))
}

func (m *MemoryStore) ListMembers(projectID string) []*ProjectMemberRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := projectID + ":"
	out := make([]*ProjectMemberRecord, 0)
	for key, rec := range m.projectMembers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			out = append(out, rec)
		}
	}
	return out
}

// ---- Users ----

func (m *MemoryStore) GetUser(userID string) (*UserRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.users[userID]
	return rec, ok
}

func (m *MemoryStore) GetUserByUsername(username string) (*UserRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.users {
		if rec.User.GetUsername() == username {
			return rec, true
		}
	}
	return nil, false
}

func (m *MemoryStore) UpdateUser(rec *UserRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[rec.User.GetUserId()] = rec
}

// CreateUser inserts a new user（V2.1 ADR-205）。username/email 任一已存在返回 false
//（唯一性契约，03 §3 ALREADY_EXISTS）。
func (m *MemoryStore) CreateUser(rec *UserRecord) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.User.GetUsername() == rec.User.GetUsername() || u.User.GetEmail() == rec.User.GetEmail() {
			return false
		}
	}
	m.users[rec.User.GetUserId()] = rec
	return true
}

// ListUsers 返回按 user_id 排序的用户（确定性分页基准，同 ListProjects 口径），
// 按 state（UNSPECIFIED=不过滤）与用户名子串过滤（V2.1 ADR-205）。
func (m *MemoryStore) ListUsers(state v1.User_UserState, usernameContains string) []*UserRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*UserRecord, 0, len(m.users))
	for _, rec := range m.users {
		if state != v1.User_USER_STATE_UNSPECIFIED && rec.User.GetState() != state {
			continue
		}
		if usernameContains != "" && !strings.Contains(rec.User.GetUsername(), usernameContains) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User.GetUserId() < out[j].User.GetUserId() })
	return out
}

// ---- Permissions (simple in-memory role → permissions mapping) ----

var rolePermissions = map[string][]string{
	"admin":     {"project:create", "project:read", "project:update", "project:delete", "user:manage"},
	"developer": {"project:read", "project:update"},
	"viewer":    {"project:read"},
}

// GetUserPermissionList returns the permission strings for a given user based
// on their project membership roles.
func (m *MemoryStore) GetUserPermissionList(userID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	permSet := make(map[string]struct{})
	for _, rec := range m.projectMembers {
		if rec.Member.GetUserId() == userID {
			if perms, ok := rolePermissions[rec.Member.GetRole()]; ok {
				for _, p := range perms {
					permSet[p] = struct{}{}
				}
			}
		}
	}
	// If user has no project memberships, give them a default read permission
	if len(permSet) == 0 {
		permSet["project:read"] = struct{}{}
	}
	out := make([]string, 0, len(permSet))
	for p := range permSet {
		out = append(out, p)
	}
	return out
}
