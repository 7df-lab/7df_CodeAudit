package handler

// R89-R91→ADR-257 锁定测试：RBAC 层级制矩阵（2026-10-10 决议）。
// 读面（tasks/findings/reports/projects 列表+详情）=认证即读；操作面（创建/更新/
// 生命周期动作/图编排/报告生成）=DEVELOPER+（VIEWER 拒）；写判定面（verdict 回写）
// =owner 或 admin；项目删除/管理面=admin。

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

// ---- 进程内假后端（Task/Result/Project 三域，形态按生产契约） ----

type authzTaskBackend struct {
	pb.UnimplementedTaskServiceServer
	owner string // 任务 created_by
}

func (f *authzTaskBackend) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	return &pb.ScanTask{TaskId: req.GetTaskId(), CreatedBy: f.owner, Status: pb.TaskStatus_TASK_STATUS_RUNNING}, nil
}
func (f *authzTaskBackend) ListScanTasks(ctx context.Context, req *pb.ListScanTasksRequest) (*pb.ListScanTasksResponse, error) {
	return &pb.ListScanTasksResponse{}, nil
}
func (f *authzTaskBackend) CancelScanTask(ctx context.Context, req *pb.CancelScanTaskRequest) (*pb.ScanTask, error) {
	return &pb.ScanTask{TaskId: req.GetTaskId()}, nil
}

type authzResultBackend struct {
	pb.UnimplementedResultServiceServer
	pb.UnimplementedReportServiceServer
	taskID string // findings/reports 挂靠的任务
}

func (f *authzResultBackend) GetFinding(ctx context.Context, req *pb.GetFindingRequest) (*pb.AuditFinding, error) {
	return &pb.AuditFinding{Finding: &pb.UnifiedFinding{FindingId: req.GetFindingId(), TaskId: f.taskID}}, nil
}
func (f *authzResultBackend) ListFindings(ctx context.Context, req *pb.ListFindingsRequest) (*pb.ListFindingsResponse, error) {
	return &pb.ListFindingsResponse{Findings: []*pb.UnifiedFinding{{FindingId: "f-1", TaskId: req.GetTaskId()}}}, nil
}
func (f *authzResultBackend) UpdateVerdict(ctx context.Context, req *pb.UpdateVerdictRequest) (*pb.AuditFinding, error) {
	return &pb.AuditFinding{Finding: &pb.UnifiedFinding{FindingId: req.GetFindingId(), TaskId: f.taskID}}, nil
}
func (f *authzResultBackend) BatchUpdateVerdict(ctx context.Context, req *pb.BatchUpdateVerdictRequest) (*pb.BatchUpdateVerdictResponse, error) {
	return &pb.BatchUpdateVerdictResponse{UpdatedCount: int32(len(req.GetFindingIds()))}, nil
}
func (f *authzResultBackend) GetReport(ctx context.Context, req *pb.GetReportRequest) (*pb.Report, error) {
	return &pb.Report{ReportId: req.GetReportId(), TaskId: f.taskID}, nil
}
func (f *authzResultBackend) ListReports(ctx context.Context, req *pb.ListReportsRequest) (*pb.ListReportsResponse, error) {
	return &pb.ListReportsResponse{}, nil
}

type authzProjectBackend struct {
	pb.UnimplementedProjectServiceServer
	mu      sync.Mutex
	members []*pb.ProjectMember // ListProjectMembers 应答
	creator string              // CreateProject 后成员登记捕获
}

func (f *authzProjectBackend) ListProjects(ctx context.Context, req *pb.ListProjectsRequest) (*pb.ListProjectsResponse, error) {
	return &pb.ListProjectsResponse{}, nil // ADR-257：裸列表读面放开后假体需应答
}
func (f *authzProjectBackend) GetProject(ctx context.Context, req *pb.GetProjectRequest) (*pb.Project, error) {
	return &pb.Project{ProjectId: req.GetProjectId(), Name: "p"}, nil
}
func (f *authzProjectBackend) CreateProject(ctx context.Context, req *pb.CreateProjectRequest) (*pb.Project, error) {
	return &pb.Project{ProjectId: "p-new", Name: req.GetProject().GetName()}, nil
}
func (f *authzProjectBackend) AddProjectMember(ctx context.Context, req *pb.AddProjectMemberRequest) (*pb.ProjectMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creator = req.GetMember().GetUserId()
	return req.GetMember(), nil
}
func (f *authzProjectBackend) ListProjectMembers(ctx context.Context, req *pb.ListProjectMembersRequest) (*pb.ListProjectMembersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.ListProjectMembersResponse{Members: f.members}, nil
}
func (f *authzProjectBackend) UpdateProject(ctx context.Context, req *pb.UpdateProjectRequest) (*pb.Project, error) {
	return req.GetProject(), nil
}

// startAuthzBackends — 三域假体一次装配，返回转码器（不套 JWT：身份经 ctx 注入）。
func startAuthzBackends(t *testing.T, owner string, members []*pb.ProjectMember) (*Transcoder, *authzProjectBackend) {
	t.Helper()
	tlis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tasks := &authzTaskBackend{owner: owner}
	tsrv := grpc.NewServer()
	pb.RegisterTaskServiceServer(tsrv, tasks)
	go func() { _ = tsrv.Serve(tlis) }()
	t.Cleanup(tsrv.Stop)

	rlis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := &authzResultBackend{taskID: "t-owned"}
	rsrv := grpc.NewServer()
	pb.RegisterResultServiceServer(rsrv, result)
	// R90: 网关报告域走独立 ReportService——假后端双注册，归属链走真实 GetReport
	pb.RegisterReportServiceServer(rsrv, result)
	go func() { _ = rsrv.Serve(rlis) }()
	t.Cleanup(rsrv.Stop)

	plis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	project := &authzProjectBackend{members: members}
	psrv := grpc.NewServer()
	pb.RegisterProjectServiceServer(psrv, project)
	go func() { _ = psrv.Serve(plis) }()
	t.Cleanup(psrv.Stop)

	tr := NewTranscoder(BackendAddrs{
		TaskAddr:     tlis.Addr().String(),
		ResultAddr:   rlis.Addr().String(),
		ProjectAddr:  plis.Addr().String(),
		CallTimeoutS: 5,
	})
	t.Cleanup(tr.Close)
	return tr, project
}

// R89 主矩阵：任务域按 created_by 收口——owner/admin 放行，他人 403。
func TestAuthz_TaskOwnershipMatrix(t *testing.T) {
	tr, _ := startAuthzBackends(t, "user-a", nil)

	// owner 读自己的任务
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodGet, "/v1/tasks/t-owned", "")
	if code != http.StatusOK {
		t.Fatalf("owner 读任务应 200, got %d", code)
	}
	// 他人读 → 200（ADR-257：读面=认证即读，R89 一期 owner 收紧就此反转）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/tasks/t-owned", "")
	if code != http.StatusOK {
		t.Fatalf("他人读任务应 200（ADR-257）, got %d", code)
	}
	// 他人读源码上下文：门已拆（非 403 即通过门禁；400=夹具无该任务文件，内容面另测）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/tasks/t-owned/source-file", "")
	if code == http.StatusForbidden {
		t.Fatalf("他人 source-file 不应 403（ADR-257 读面放开）, got %d", code)
	}
	// 他人动作（cancel）→ 200（操作面 DEVELOPER+，层级制不按 owner 收口）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodPost, "/v1/tasks/t-owned/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("他人 cancel 应 200（操作面 DEVELOPER+）, got %d", code)
	}
	// VIEWER：读放行、动作拒（RBAC 三角色分层）
	code, _ = httpJSONAsUser(t, tr, "ROLE_VIEWER", "user-v", http.MethodGet, "/v1/tasks/t-owned", "")
	if code != http.StatusOK {
		t.Fatalf("VIEWER 读任务应 200, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_VIEWER", "user-v", http.MethodPost, "/v1/tasks/t-owned/cancel", "")
	if code != http.StatusForbidden {
		t.Fatalf("VIEWER cancel 应 403, got %d", code)
	}
	// admin 全权
	code, _ = httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodGet, "/v1/tasks/t-owned", "")
	if code != http.StatusOK {
		t.Fatalf("admin 读任务应 200, got %d", code)
	}
}

// R90 矩阵：finding/report 按 ID → 解析 task → 归属；列表带 task_id 走归属、裸列表 admin。
func TestAuthz_FindingReportMatrix(t *testing.T) {
	tr, _ := startAuthzBackends(t, "user-a", nil)

	cases := []struct {
		name, path string
	}{
		{"他人读 finding", "/v1/findings/f-1"},
		{"他人读报告", "/v1/reports/r-1"},
		{"他人带 task_id 列 findings", "/v1/findings?task_id=t-owned"},
	}
	for _, tc := range cases {
		code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, tc.path, "")
		if code != http.StatusOK {
			t.Fatalf("%s: want 200（ADR-257 读面放开）, got %d", tc.name, code)
		}
	}
	// 写判定面不随读面反转：他人 verdict 回写仍 owner/admin
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodPut, "/v1/findings/f-1/verdict", "{}")
	if code != http.StatusForbidden {
		t.Fatalf("他人回写 verdict: want 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_VIEWER", "user-v", http.MethodPut, "/v1/findings/f-1/verdict", "{}")
	if code != http.StatusForbidden {
		t.Fatalf("VIEWER 回写 verdict: want 403, got %d", code)
	}
	// owner 放行（finding/report 均挂靠 t-owned=owner 任务）
	for _, path := range []string{"/v1/findings/f-1", "/v1/findings?task_id=t-owned",
		"/v1/reports/r-1", "/v1/reports?task_id=t-owned"} {
		code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("owner %s 应 200, got %d", path, code)
		}
	}
	// 裸列表：认证即读（ADR-257；R90 一期 admin 收紧反转）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/findings", "")
	if code != http.StatusOK {
		t.Fatalf("裸 findings 列表非 admin 应 200（ADR-257）, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodGet, "/v1/findings", "")
	if code != http.StatusOK {
		t.Fatalf("admin 裸列表应 200, got %d", code)
	}
	// 批量 triage 与单条同口径（单条收口而批量直通=门禁旁路）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodPost,
		"/v1/findings/verdict:batch", `{"finding_ids":["f-1","f-2"],"verdict":"AI_VERDICT_FALSE_POSITIVE"}`)
	if code != http.StatusForbidden {
		t.Fatalf("他人批量 verdict 应 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodPost,
		"/v1/findings/verdict:batch", `{"finding_ids":["f-1","f-1","f-2"],"verdict":"AI_VERDICT_FALSE_POSITIVE"}`)
	if code != http.StatusOK {
		t.Fatalf("owner 批量 verdict 应 200, got %d", code)
	}
}

// R91 矩阵：项目域——读=成员或 admin，写=admin；创建者自动登记为成员。
func TestAuthz_ProjectMembershipMatrix(t *testing.T) {
	members := []*pb.ProjectMember{{ProjectId: "p-owned", UserId: "user-a", Role: "developer"}}
	tr, project := startAuthzBackends(t, "user-a", members)

	// 读：认证即读（ADR-257；R91 成员制读面就此让位，成员基建留作后续资源粒度制）
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodGet, "/v1/projects/p-owned", "")
	if code != http.StatusOK {
		t.Fatalf("成员读项目应 200, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/projects/p-owned", "")
	if code != http.StatusOK {
		t.Fatalf("非成员读项目应 200（ADR-257）, got %d", code)
	}
	// 更新：操作面 DEVELOPER+（R91 一期 admin-only 放宽）；删除仍 admin
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodPut, "/v1/projects/p-owned",
		`{"project":{"project_id":"p-owned","name":"renamed"}}`)
	if code != http.StatusOK {
		t.Fatalf("成员更新项目应 200（操作面 DEVELOPER+）, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodDelete, "/v1/projects/p-owned", "")
	if code != http.StatusForbidden {
		t.Fatalf("非 admin 删除项目应 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_VIEWER", "user-v", http.MethodPut, "/v1/projects/p-owned",
		`{"project":{"project_id":"p-owned","name":"renamed"}}`)
	if code != http.StatusForbidden {
		t.Fatalf("VIEWER 更新项目应 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodPut, "/v1/projects/p-owned",
		`{"project":{"project_id":"p-owned","name":"renamed"}}`)
	if code != http.StatusOK {
		t.Fatalf("admin 写项目应 200, got %d", code)
	}
	// 裸列表：认证即读（ADR-257）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/projects", "")
	if code != http.StatusOK {
		t.Fatalf("裸项目列表非 admin 应 200（ADR-257）, got %d", code)
	}
	// 创建者自动登记：u-a 建项目 → AddProjectMember(user-a) 被调用
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodPost, "/v1/projects",
		`{"project":{"name":"fresh"}}`)
	if code != http.StatusOK {
		t.Fatalf("创建项目应 200, got %d", code)
	}
	// VIEWER 建项目：403（操作面）
	code, _ = httpJSONAsUser(t, tr, "ROLE_VIEWER", "user-v", http.MethodPost, "/v1/projects",
		`{"project":{"name":"nope"}}`)
	if code != http.StatusForbidden {
		t.Fatalf("VIEWER 创建项目应 403, got %d", code)
	}
	project.mu.Lock()
	got := project.creator
	project.mu.Unlock()
	if got != "user-a" {
		t.Fatalf("创建者未登记为成员: creator=%q", got)
	}
}
