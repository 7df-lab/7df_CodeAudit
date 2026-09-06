package service

// ADR-209 优先级回归锁：任务源解析链必须是
//   任务 config.upload_file_id → 项目 config.upload_file_id → 项目 repo_url clone
// 此前 L312(repo 分支)守卫缺 r.Prepare == nil，前两档解析出的 storage 拉包闭包被
// 克隆闭包无条件覆盖（任务级/项目级双雷；项目级因用例恰好未配 repo_url 而侥幸未爆）。
//
// 判别法：CODEAUDIT_STORAGE_ADDR 指向不可达端口、项目假服务返回 repo_url——
// 修复前任务 DEAD 且 error 含 "git clone"（被覆盖成克隆）；修复后走 storage 通道，
// DEAD/FAILED 的错误属上传件拉取（绝不出现 "git clone"）。

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/codeaudit/proto-gen"
)

// fakeProjectServer — 最小 ProjectService：按注入值返回 repo 与项目 config。
type fakeProjectServer struct {
	pb.UnimplementedProjectServiceServer
	repoURL  string
	branch   string
	cfgValue map[string]string // GetProjectConfig 返回的 config map
}

func (f *fakeProjectServer) GetProject(ctx context.Context, req *pb.GetProjectRequest) (*pb.Project, error) {
	return &pb.Project{ProjectId: req.GetProjectId(), RepoUrl: f.repoURL, DefaultBranch: f.branch}, nil
}

func (f *fakeProjectServer) GetProjectConfig(ctx context.Context, req *pb.GetProjectConfigRequest) (*pb.ProjectConfig, error) {
	return &pb.ProjectConfig{ProjectId: req.GetProjectId(), Config: f.cfgValue}, nil
}

// startFakeProject — 起真 gRPC 假 project-service，返回地址。
func startFakeProject(t *testing.T, f *fakeProjectServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterProjectServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// startTaskWithSource — 建任务(可带任务级 config)并启动，等待终态，返回任务。
func startTaskWithSource(t *testing.T, s *TaskServiceImpl, id string, cfg map[string]string) *pb.ScanTask {
	t.Helper()
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata: &pb.RequestMetadata{RequestId: id}, ProjectId: "p-" + id,
		ScanMode: pb.ScanMode_SCAN_MODE_SAST_ONLY, Config: cfg,
	})
	if err != nil {
		t.Fatalf("CreateScanTask: %v", err)
	}
	if _, err := s.StartTask(context.Background(), &pb.StartTaskRequest{TaskId: task.GetTaskId()}); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	return waitForStatus(t, s, task.GetTaskId(), pb.TaskStatus_TASK_STATUS_DEAD, 30*time.Second)
}

// TestStartTask_TaskUploadWinsOverRepoURL — 任务级上传件必须压过项目 repo_url(ADR-209 红转绿)。
func TestStartTask_TaskUploadWinsOverRepoURL(t *testing.T) {
	addr := startFakeProject(t, &fakeProjectServer{repoURL: "https://demo.example/x.git", branch: "main"})
	t.Setenv("CODEAUDIT_PROJECT_ADDR", addr)
	t.Setenv("CODEAUDIT_STORAGE_ADDR", "127.0.0.1:59993") // 不可达:storage 通道必失败,但失败必须属于 storage
	s := newSvc(t)
	task := startTaskWithSource(t, s, "prio-task", map[string]string{"upload_file_id": "file-prio-1"})
	if em := task.GetErrorMessage(); em == "" || containsGitClone(em) {
		t.Fatalf("任务级 upload_file_id 被 repo_url 抢跑(ADR-209 回归): error=%q", em)
	}
}

// TestStartTask_ProjectUploadWinsOverRepoURL — 项目级上传件同样必须压过项目 repo_url(第二颗潜伏雷)。
func TestStartTask_ProjectUploadWinsOverRepoURL(t *testing.T) {
	addr := startFakeProject(t, &fakeProjectServer{
		repoURL: "https://demo.example/x.git", branch: "main",
		cfgValue: map[string]string{"upload_file_id": "file-prio-2"},
	})
	t.Setenv("CODEAUDIT_PROJECT_ADDR", addr)
	t.Setenv("CODEAUDIT_STORAGE_ADDR", "127.0.0.1:59993")
	s := newSvc(t)
	task := startTaskWithSource(t, s, "prio-proj", nil)
	if em := task.GetErrorMessage(); em == "" || containsGitClone(em) {
		t.Fatalf("项目级 upload_file_id 被 repo_url 抢跑(ADR-209 回归): error=%q", em)
	}
}

// TestStartTask_RepoURLStillFallback — 三档皆空前置 + 项目仅 repo_url 时,克隆仍是合法兜底
// (守卫收紧不得误伤第三档;容器内无 git,故错误应含 git clone——这正是本测试期望)。
func TestStartTask_RepoURLStillFallback(t *testing.T) {
	addr := startFakeProject(t, &fakeProjectServer{repoURL: "https://demo.example/x.git", branch: "main"})
	t.Setenv("CODEAUDIT_PROJECT_ADDR", addr)
	s := newSvc(t)
	task := startTaskWithSource(t, s, "prio-repo", nil)
	if em := task.GetErrorMessage(); !containsGitClone(em) {
		t.Fatalf("repo_url 兜底档丢失(ADR-209 守卫收紧误伤): error=%q", em)
	}
}

func containsGitClone(s string) bool { return strings.Contains(s, "git clone") }
