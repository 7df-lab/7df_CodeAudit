package handler

// ADR-248 L4 契约锁定：POST /v1/tasks body 的 authorized_targets（任务【资源】
// 字段）经 gateway protojson 透传到 CreateScanTask 实收请求——REST→gRPC 契约面
// （进程内真实 gRPC 后端捕获，inferenceCaptureBackend 同型）。

import (
	"context"
	"net"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

type tasksCaptureBackend struct {
	pb.UnimplementedTaskServiceServer
	addr  string
	srv   *grpc.Server
	mu    sync.Mutex
	got   *pb.CreateScanTaskRequest
	roles map[string]string // 覆盖 canAccessTask 的 project 查询？——POST /v1/tasks 无归属核验（创建即本人）
}

func startTasksCapture(t *testing.T) *tasksCaptureBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &tasksCaptureBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterTaskServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *tasksCaptureBackend) CreateScanTask(ctx context.Context, req *pb.CreateScanTaskRequest) (*pb.ScanTask, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = req
	return &pb.ScanTask{TaskId: req.GetMetadata().GetRequestId(), Status: pb.TaskStatus_TASK_STATUS_CREATED,
		AuthorizedTargets: req.GetAuthorizedTargets()}, nil
}

func TestTasksCreate_AuthorizedTargetsPassthrough(t *testing.T) {
	b := startTasksCapture(t)
	tr := NewTranscoder(BackendAddrs{TaskAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, out := httpJSON(t, tr, "POST", "/v1/tasks",
		`{"project_id":"p1","scan_mode":"SCAN_MODE_SAST_ONLY","authorized_targets":[{"host":"t.example","port":443,"allowed_ips":["203.0.113.7"]}]}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	ats, _ := out["authorized_targets"].([]any)
	if len(ats) != 1 {
		t.Fatalf("响应应回显 authorized_targets: %v", out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.got.GetAuthorizedTargets()) != 1 {
		t.Fatalf("后端实收 authorized_targets 为空（透传断链）: %v", b.got)
	}
	at := b.got.GetAuthorizedTargets()[0]
	if at.GetHost() != "t.example" || at.GetPort() != 443 || len(at.GetAllowedIps()) != 1 {
		t.Fatalf("三元组透传形状: %+v", at)
	}
}
