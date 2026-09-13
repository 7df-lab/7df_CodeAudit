package handler

// R89-R91 锁定测试：资源归属授权矩阵（水平越权收口）。
// owner（user-a，资源 created_by/成员）与 admin 放行；他人（user-b）403 且门禁
// 在转发前拦截。列表接口：带 task_id 走归属，裸列表仅 admin。

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
	// 他人读 → 403
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/tasks/t-owned", "")
	if code != http.StatusForbidden {
		t.Fatalf("他人读任务应 403, got %d", code)
	}
	// 他人动作（cancel）→ 403
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodPost, "/v1/tasks/t-owned/cancel", "")
	if code != http.StatusForbidden {
		t.Fatalf("他人 cancel 应 403, got %d", code)
	}
	// 他人读源码上下文 → 403（内容面）
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/tasks/t-owned/source-file", "")
	if code != http.StatusForbidden {
		t.Fatalf("他人 source-file 应 403, got %d", code)
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
		{"他人回写 verdict", "/v1/findings/f-1/verdict"},
		{"他人读报告", "/v1/reports/r-1"},
		{"他人带 task_id 列 findings", "/v1/findings?task_id=t-owned"},
	}
	for _, tc := range cases {
		method := http.MethodGet
		if tc.name == "他人回写 verdict" {
			method = http.MethodPut
		}
		code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", method, tc.path, "")
		if code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", tc.name, code)
		}
	}
	// owner 放行（finding/report 均挂靠 t-owned=owner 任务）
	for _, path := range []string{"/v1/findings/f-1", "/v1/findings?task_id=t-owned",
		"/v1/reports/r-1", "/v1/reports?task_id=t-owned"} {
		code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("owner %s 应 200, got %d", path, code)
		}
	}
	// 裸列表：非 admin 403、admin 200
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/findings", "")
	if code != http.StatusForbidden {
		t.Fatalf("裸 findings 列表非 admin 应 403, got %d", code)
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

	// 读：成员放行、非成员 403
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodGet, "/v1/projects/p-owned", "")
	if code != http.StatusOK {
		t.Fatalf("成员读项目应 200, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/projects/p-owned", "")
	if code != http.StatusForbidden {
		t.Fatalf("非成员读项目应 403, got %d", code)
	}
	// 写：成员（非 admin）403、admin 200
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodPut, "/v1/projects/p-owned",
		`{"project":{"project_id":"p-owned","name":"renamed"}}`)
	if code != http.StatusForbidden {
		t.Fatalf("成员写项目应 403（写面=管理面）, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodPut, "/v1/projects/p-owned",
		`{"project":{"project_id":"p-owned","name":"renamed"}}`)
	if code != http.StatusOK {
		t.Fatalf("admin 写项目应 200, got %d", code)
	}
	// 裸列表：非 admin 403
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-b", http.MethodGet, "/v1/projects", "")
	if code != http.StatusForbidden {
		t.Fatalf("裸项目列表非 admin 应 403, got %d", code)
	}
	// 创建者自动登记：u-a 建项目 → AddProjectMember(user-a) 被调用
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "user-a", http.MethodPost, "/v1/projects",
		`{"project":{"name":"fresh"}}`)
	if code != http.StatusOK {
		t.Fatalf("创建项目应 200, got %d", code)
	}
	project.mu.Lock()
	got := project.creator
	project.mu.Unlock()
	if got != "user-a" {
		t.Fatalf("创建者未登记为成员: creator=%q", got)
	}
}
