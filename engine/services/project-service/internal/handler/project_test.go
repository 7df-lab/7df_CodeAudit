package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	store := repo.NewMemoryStore(true)
	idm := idempotency.New()
	svc := service.NewProjectService(store)
	return handler.NewProjectHandler(svc, idm)
}

func setupUserHandler(t *testing.T) *handler.UserHandler {
	t.Helper()
	t.Setenv("CODEAUDIT_JWT_SECRET", "test-secret-r65") // R65: jwtSecret fail-fast 后测试须显式供密钥
	store := repo.NewMemoryStore(true)
	idm := idempotency.New()
	svc := service.NewUserService(store)
	return handler.NewUserHandler(svc, idm)
}

// wantCode — 断言 err 携带期望的 gRPC 错误码（status.FromError 样板收敛）。
func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("want %v, got %v (err=%v)", want, status.Code(err), err)
	}
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
	wantCode(t, err, codes.AlreadyExists)
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
	wantCode(t, err, codes.InvalidArgument)
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
	wantCode(t, err, codes.InvalidArgument)
}

func TestGetProject_NotFound(t *testing.T) {
	h := setupProjectHandler()

	_, err := h.GetProject(context.Background(), &v1.GetProjectRequest{
		ProjectId: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected NotFound error, got nil")
	}
	wantCode(t, err, codes.NotFound)
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
	h := setupUserHandler(t)

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
	h := setupUserHandler(t)

	_, err := h.Login(context.Background(), &v1.LoginRequest{
		Username: "admin",
		Password: "wrong-password",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated error, got nil")
	}
	wantCode(t, err, codes.Unauthenticated)
}

func TestLogin_NonexistentUser(t *testing.T) {
	h := setupUserHandler(t)

	_, err := h.Login(context.Background(), &v1.LoginRequest{
		Username: "nobody",
		Password: "password",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated error, got nil")
	}
}

func TestGetUser(t *testing.T) {
	h := setupUserHandler(t)

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
	h := setupUserHandler(t)

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
	h := setupUserHandler(t)

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

// R-32 锁定测试：CreateProject 缺 project 包装键/空 name 必须 400（InvalidArgument），
// 禁止静默创建全空项目（dind 全新环境实测：裸 {"name":...} 顶层载荷 201 空壳）。
// 校验先于 idm/svc 触达，零值 handler 即可离线验证。
func TestCreateProjectRejectsMissingProject(t *testing.T) {
	h := &handler.ProjectHandler{} // 校验先于依赖触达——nil svc/idm 安全
	cases := []struct {
		desc string
		req  *v1.CreateProjectRequest
	}{
		{"缺 project 包装键", &v1.CreateProjectRequest{}},
		{"project 为 nil", &v1.CreateProjectRequest{Project: nil}},
		{"project.name 为空", &v1.CreateProjectRequest{Project: &v1.Project{Name: ""}}},
		{"project.name 全空白", &v1.CreateProjectRequest{Project: &v1.Project{Name: "   "}}},
	}
	for _, tc := range cases {
		_, err := h.CreateProject(context.Background(), tc.req)
		if err == nil {
			t.Fatalf("%s: 未拒绝（期望 InvalidArgument）", tc.desc)
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: 错误码=%v 期望 InvalidArgument", tc.desc, status.Code(err))
		}
		// 消息锚定 R-32 校验本体：紧随其后的 R-4 metadata 校验同返回
		// InvalidArgument，只看错误码无法区分二者（M28 变异会借道存活）。
		if !strings.Contains(status.Convert(err).Message(), "project is required") {
			t.Fatalf("%s: 消息=%q 未锚定包装键校验（疑似命中后续校验）", tc.desc, status.Convert(err).Message())
		}
	}
}

// R74 锁定测试：repo_url scheme 白名单 + 选项注入拒绝——git `ext::<command>` 外置
// 传输会在 task-service 容器内执行任意命令（认证后 RCE）；前导 '-' 会被 git 当作
// 选项（参数走私）。空 repo_url 合法（上传模式）。校验先于依赖触达，零值 handler 可离线验证。
func TestCreateProject_RejectsUnsafeRepoURL(t *testing.T) {
	h := &handler.ProjectHandler{}
	cases := []struct{ desc, repoURL string }{
		{"git ext:: 外置传输 RCE", "ext::sh -c touch /tmp/pwned"},
		{"前导空白使 parse 短路", " ext::sh -c id"},
		{"前导 '-' 选项走私", "-oProxyCommand=evil"},
		{"白名单外 scheme", "ftp://example.com/x.git"},
	}
	for _, tc := range cases {
		_, err := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
			Metadata: &v1.RequestMetadata{RequestId: "req-r74-" + tc.desc},
			Project:  &v1.Project{Name: "p", RepoUrl: tc.repoURL},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: repoURL=%q 错误码=%v, 期望 InvalidArgument", tc.desc, tc.repoURL, status.Code(err))
		}
		if !strings.Contains(status.Convert(err).Message(), "repo_url") {
			t.Fatalf("%s: 错误未锚定 repo_url 校验本体: %v", tc.desc, err)
		}
	}
	// default_branch 前导 '-' 同面拒绝（clone -b <branch> 选项走私）
	_, err := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-r74-branch"},
		Project:  &v1.Project{Name: "p", RepoUrl: "https://git.example/x.git", DefaultBranch: "-u exec"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("default_branch 前导 '-': 错误码=%v, 期望 InvalidArgument", status.Code(err))
	}
}

// R74: 合法 scheme（http/https/ssh/git/file，file 为 V1 凭据边界声明支持的形态）放行。
func TestCreateProject_AllowedRepoURLSchemes(t *testing.T) {
	h := setupProjectHandler()
	for i, u := range []string{
		"https://git.example/x.git",
		"http://git.internal/x.git",
		"ssh://git@git.example/x.git",
		"git@host:x.git",
		"file:///data/repos/x",
	} {
		resp, err := h.CreateProject(context.Background(), &v1.CreateProjectRequest{
			Metadata: &v1.RequestMetadata{RequestId: fmt.Sprintf("req-r74-ok-%d", i)},
			Project:  &v1.Project{Name: fmt.Sprintf("p-%d", i), RepoUrl: u},
		})
		if err != nil {
			t.Fatalf("scheme %q 被误拒: %v", u, err)
		}
		if resp.GetRepoUrl() != u {
			t.Fatalf("repo_url 回读不一致: %q", resp.GetRepoUrl())
		}
	}
}

// R74: UpdateProject 通道同样过白名单（存量项目被改写为 ext:: 不得绕过）。
func TestUpdateProject_RejectsUnsafeRepoURL(t *testing.T) {
	h := &handler.ProjectHandler{}
	_, err := h.UpdateProject(context.Background(), &v1.UpdateProjectRequest{
		Project: &v1.Project{ProjectId: "proj-x", Name: "p", RepoUrl: "ext::sh -c id"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("UpdateProject ext:: 错误码=%v, 期望 InvalidArgument", status.Code(err))
	}
}

// R85（复审）: UpdateUser 对缺省 state（UNSPECIFIED）保全存量——网关非 admin self
// 剥离 state 后依赖此语义（防被停用户自复活），且修复"未带 state 字段的更新把
// state 写成 0"的既有清零面。
func TestUpdateUser_PreservesStateWhenUnspecified(t *testing.T) {
	store := repo.NewMemoryStore(true)
	svc := service.NewUserService(store)
	// 种子 user-001 state=ACTIVE；模拟"只改 email、不带 state"的更新
	_, ok := svc.UpdateUser(&v1.User{UserId: "user-001", Email: "new@x"})
	if !ok {
		t.Fatal("UpdateUser failed")
	}
	rec, _ := store.GetUser("user-001")
	if rec.User.GetState() != v1.User_USER_STATE_ACTIVE {
		t.Fatalf("state 被缺省更新清零: %v, want ACTIVE（R85 保全缺失）", rec.User.GetState())
	}
}
