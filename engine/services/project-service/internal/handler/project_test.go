package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/handler"
	"github.com/codeaudit/services/project-service/internal/idempotency"
	"github.com/codeaudit/services/project-service/internal/repo"
	"github.com/codeaudit/services/project-service/internal/service"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// helpers

func setupProjectHandler() *handler.ProjectHandler {
	store := repo.NewMemoryStore()
	idm := idempotency.New()
	svc := service.NewProjectService(store)
	return handler.NewProjectHandler(svc, idm)
}

func setupUserHandler() *handler.UserHandler {
	store := repo.NewMemoryStore()
	idm := idempotency.New()
	svc := service.NewUserService(store)
	return handler.NewUserHandler(svc, idm)
}

// ---- ProjectService Tests ----

func TestCreateProject_Basic(t *testing.T) {
	h := setupProjectHandler()

	req := &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{
			RequestId: "req-001",
		},
		Project: &v1.Project{
			Name:    "test-project",
			RepoUrl: "https://github.com/example/repo",
		},
	}

	resp, err := h.CreateProject(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	if resp.GetName() != "test-project" {
		t.Errorf("expected name 'test-project', got '%s'", resp.GetName())
	}
	if resp.GetProjectId() == "" {
		t.Error("expected non-empty project_id")
	}
	if resp.GetCreatedAt() == nil {
		t.Error("expected non-nil created_at")
	}
}

func TestIdempotency_SameKeySameBody_ReturnsSameResult(t *testing.T) {
	h := setupProjectHandler()

	req := &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{
			RequestId: "req-idempotent-001",
		},
		Project: &v1.Project{
			Name:    "idempotent-project",
			RepoUrl: "https://github.com/example/idempotent",
		},
	}

	// First call
	resp1, err := h.CreateProject(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreateProject failed: %v", err)
	}

	// Second call with same key + same body
	resp2, err := h.CreateProject(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateProject failed: %v", err)
	}

	// Must return the same project_id (idempotent replay)
	if resp1.GetProjectId() != resp2.GetProjectId() {
		t.Errorf("idempotency failed: got different project_ids %s vs %s",
			resp1.GetProjectId(), resp2.GetProjectId())
	}
}

func TestIdempotency_SameKeyDifferentBody_ReturnsAlreadyExists(t *testing.T) {
	h := setupProjectHandler()

	// First request
	req1 := &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{
			RequestId: "req-conflict-001",
		},
		Project: &v1.Project{
			Name:    "project-a",
			RepoUrl: "https://github.com/example/a",
		},
	}

	_, err := h.CreateProject(context.Background(), req1)
	if err != nil {
		t.Fatalf("first CreateProject failed: %v", err)
	}

	// Second request with same key but different body
	req2 := &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{
			RequestId: "req-conflict-001", // same key
		},
		Project: &v1.Project{
			Name:    "project-b", // different body
			RepoUrl: "https://github.com/example/b",
		},
	}

	_, err = h.CreateProject(context.Background(), req2)
	if err == nil {
		t.Fatal("expected ALREADY_EXISTS error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.AlreadyExists {
		t.Errorf("expected ALREADY_EXISTS(9), got %s(%d)", st.Code(), st.Code())
	}
}

func TestMissingMetadata_ReturnsInvalidArgument(t *testing.T) {
	h := setupProjectHandler()

	req := &v1.CreateProjectRequest{
		// Metadata is nil — should fail
		Project: &v1.Project{
			Name: "no-metadata-project",
		},
	}

	_, err := h.CreateProject(context.Background(), req)
	if err == nil {
		t.Fatal("expected INVALID_ARGUMENT error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT(3), got %s(%d)", st.Code(), st.Code())
	}
}

func TestMissingMetadataRequestId_ReturnsInvalidArgument(t *testing.T) {
	h := setupProjectHandler()

	req := &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{
			// RequestId is empty
		},
		Project: &v1.Project{
			Name: "empty-request-id",
		},
	}

	_, err := h.CreateProject(context.Background(), req)
	if err == nil {
		t.Fatal("expected INVALID_ARGUMENT error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT(3), got %s(%d)", st.Code(), st.Code())
	}
}

func TestGetProject_NotFound(t *testing.T) {
	h := setupProjectHandler()

	_, err := h.GetProject(context.Background(), &v1.GetProjectRequest{
		ProjectId: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected NotFound error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %s", st.Code())
	}
}

func TestCreateAndGetProject(t *testing.T) {
	h := setupProjectHandler()

	createResp, err := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-create-get-001"},
		Project:  &v1.Project{Name: "roundtrip-project"},
	})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	getResp, err := h.GetProject(context.Background(), &v1.GetProjectRequest{
		ProjectId: createResp.GetProjectId(),
	})
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}

	if getResp.GetName() != "roundtrip-project" {
		t.Errorf("expected 'roundtrip-project', got '%s'", getResp.GetName())
	}
}

func TestUpdateProject(t *testing.T) {
	h := setupProjectHandler()

	createResp, _ := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-update-001"},
		Project:  &v1.Project{Name: "before-update"},
	})

	updateResp, err := h.UpdateProject(context.Background(), &v1.UpdateProjectRequest{
		Project: &v1.Project{
			ProjectId: createResp.GetProjectId(),
			Name:      "after-update",
		},
	})
	if err != nil {
		t.Fatalf("UpdateProject failed: %v", err)
	}

	if updateResp.GetName() != "after-update" {
		t.Errorf("expected 'after-update', got '%s'", updateResp.GetName())
	}
}

func TestDeleteProject(t *testing.T) {
	h := setupProjectHandler()

	createResp, _ := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-delete-001"},
		Project:  &v1.Project{Name: "to-delete"},
	})

	_, err := h.DeleteProject(context.Background(), &v1.DeleteProjectRequest{
		ProjectId: createResp.GetProjectId(),
	})
	if err != nil {
		t.Fatalf("DeleteProject failed: %v", err)
	}

	// Verify deletion
	_, err = h.GetProject(context.Background(), &v1.GetProjectRequest{
		ProjectId: createResp.GetProjectId(),
	})
	if err == nil {
		t.Fatal("expected NotFound after delete, got nil")
	}
}

func TestListProjects(t *testing.T) {
	h := setupProjectHandler()

	// Create a few projects
	for i := 0; i < 3; i++ {
		h.CreateProject(context.Background(), &v1.CreateProjectRequest{
			Metadata: &v1.RequestMetadata{RequestId: "req-list-" + string(rune('a'+i))},
			Project:  &v1.Project{Name: "project-" + string(rune('a'+i))},
		})
	}

	resp, err := h.ListProjects(context.Background(), &v1.ListProjectsRequest{})
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}

	if len(resp.GetProjects()) != 3 {
		t.Errorf("expected 3 projects, got %d", len(resp.GetProjects()))
	}
}

func TestAddAndListProjectMembers(t *testing.T) {
	h := setupProjectHandler()

	// Create a project first
	projResp, _ := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-member-proj"},
		Project:  &v1.Project{Name: "member-test-project"},
	})

	// Add a member
	_, err := h.AddProjectMember(context.Background(), &v1.AddProjectMemberRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-add-member-001"},
		Member: &v1.ProjectMember{
			ProjectId: projResp.GetProjectId(),
			UserId:    "user-001",
			Role:      "developer",
		},
	})
	if err != nil {
		t.Fatalf("AddProjectMember failed: %v", err)
	}

	// List members
	listResp, err := h.ListProjectMembers(context.Background(), &v1.ListProjectMembersRequest{
		ProjectId: projResp.GetProjectId(),
	})
	if err != nil {
		t.Fatalf("ListProjectMembers failed: %v", err)
	}

	if len(listResp.GetMembers()) != 1 {
		t.Errorf("expected 1 member, got %d", len(listResp.GetMembers()))
	}
	if listResp.GetMembers()[0].GetUserId() != "user-001" {
		t.Errorf("expected user-001, got %s", listResp.GetMembers()[0].GetUserId())
	}
}

func TestIdempotencyBodyHash(t *testing.T) {
	// Verify that BodyHash is deterministic
	a := &v1.Project{Name: "test"}
	b1, _ := json.Marshal(a)
	b2, _ := json.Marshal(a)

	h1 := idempotency.BodyHash(b1)
	h2 := idempotency.BodyHash(b2)

	if h1 != h2 {
		t.Errorf("BodyHash not deterministic: %s != %s", h1, h2)
	}
}

// ---- UserService Tests ----

func TestLogin_ReturnsValidJWT(t *testing.T) {
	h := setupUserHandler()

	resp, err := h.Login(context.Background(), &v1.LoginRequest{
		Username: "admin",
		Password: "admin",
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	if resp.GetAccessToken() == "" {
		t.Error("expected non-empty access_token")
	}
	if resp.GetRefreshToken() == "" {
		t.Error("expected non-empty refresh_token")
	}
	if resp.GetExpiresInS() <= 0 {
		t.Error("expected positive expires_in_s")
	}

	// Verify JWT has three dot-separated parts (header.payload.signature)
	parts := countJWTSections(resp.GetAccessToken())
	if parts != 3 {
		t.Errorf("expected 3 JWT sections (HS256), got %d", parts)
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	h := setupUserHandler()

	_, err := h.Login(context.Background(), &v1.LoginRequest{
		Username: "admin",
		Password: "wrong-password",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", st.Code())
	}
}

func TestLogin_NonexistentUser(t *testing.T) {
	h := setupUserHandler()

	_, err := h.Login(context.Background(), &v1.LoginRequest{
		Username: "nobody",
		Password: "password",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated error, got nil")
	}
}

func TestGetUser(t *testing.T) {
	h := setupUserHandler()

	user, err := h.GetUser(context.Background(), &v1.GetUserRequest{
		UserId: "user-001",
	})
	if err != nil {
		t.Fatalf("GetUser failed: %v", err)
	}
	if user.GetUsername() != "admin" {
		t.Errorf("expected 'admin', got '%s'", user.GetUsername())
	}
}

func TestValidatePermission(t *testing.T) {
	h := setupUserHandler()

	resp, err := h.ValidatePermission(context.Background(), &v1.ValidatePermissionRequest{
		UserId:       "user-001",
		ResourceType: "project",
		ResourceId:   "proj-001",
		Action:       "read",
	})
	if err != nil {
		t.Fatalf("ValidatePermission failed: %v", err)
	}

	// user-001 has no project memberships, so default "project:read" should be granted
	if !resp.GetAllowed() {
		t.Errorf("expected allowed=true for default read, got false; reason: %s", resp.GetReason())
	}
}

func TestGetUserPermissions(t *testing.T) {
	h := setupUserHandler()

	resp, err := h.GetUserPermissions(context.Background(), &v1.GetUserPermissionsRequest{
		UserId: "user-001",
	})
	if err != nil {
		t.Fatalf("GetUserPermissions failed: %v", err)
	}

	if len(resp.GetPermissions()) == 0 {
		t.Error("expected at least one permission")
	}
}

// countJWTSections counts dot-separated sections in a JWT string.
func countJWTSections(token string) int {
	count := 1
	for _, c := range token {
		if c == '.' {
			count++
		}
	}
	return count
}

// TestListProjects_NewestFirst — ADR-161：列表按 created_at 降序（与任务/报告同一
// "最新活动优先"口径）。此前按存储插入序（最老在前），项目数超过前端分页大小时
// 新建项目落在末页——用户"创建成功却在列表看不见"。
func TestListProjects_NewestFirst(t *testing.T) {
	h := setupProjectHandler()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Millisecond) // 保证 created_at 可区分
		if _, err := h.CreateProject(ctx, &v1.CreateProjectRequest{
			Metadata: &v1.RequestMetadata{RequestId: fmt.Sprintf("req-newfirst-%d", i)},
			Project:  &v1.Project{Name: fmt.Sprintf("nf-%d", i)},
		}); err != nil {
			t.Fatalf("CreateProject %d: %v", i, err)
		}
	}
	resp, err := h.ListProjects(ctx, &v1.ListProjectsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetProjects()) != 3 {
		t.Fatalf("want 3 projects, got %d", len(resp.GetProjects()))
	}
	if got := resp.GetProjects()[0].GetName(); got != "nf-2" {
		t.Fatalf("newest project should be first, got %q", got)
	}
}
