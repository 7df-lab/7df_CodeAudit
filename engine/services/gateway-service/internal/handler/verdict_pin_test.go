package handler

// 第四轮审计 P1（越权）锁定测试：PUT /v1/findings/{id}/verdict 的 finding_id
// 必须以路径段为准——protojson.Unmarshal 会 Reset 预填值，body 携带 finding_id
// 若被采信即可"用自己有权的目标过归属门禁、实际改写他人 finding"（R89/R73
// "归属判定只用路径参数，禁信 body 字段"的执行面）。同型路由（report 生成/
// agent 动作/projects PUT）均已在 decode 后无条件钉死，本测试补齐 verdict 面。

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

type verdictPinBackend struct {
	pb.UnimplementedResultServiceServer
	pb.UnimplementedReportServiceServer
	pb.UnimplementedTaskServiceServer
	srv *grpc.Server
	mu  sync.Mutex
	got *pb.UpdateVerdictRequest
}

func (b *verdictPinBackend) GetFinding(ctx context.Context, req *pb.GetFindingRequest) (*pb.AuditFinding, error) {
	// 路径段 finding（f-legit）挂靠任务 t-own，owner=user-a（与请求身份一致=门禁放行）
	return &pb.AuditFinding{Finding: &pb.UnifiedFinding{FindingId: req.GetFindingId(), TaskId: "t-own"}}, nil
}

func (b *verdictPinBackend) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	return &pb.ScanTask{TaskId: req.GetTaskId(), CreatedBy: "user-a", Status: pb.TaskStatus_TASK_STATUS_RUNNING}, nil
}

func (b *verdictPinBackend) UpdateVerdict(ctx context.Context, req *pb.UpdateVerdictRequest) (*pb.AuditFinding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = req
	return &pb.AuditFinding{Finding: &pb.UnifiedFinding{FindingId: req.GetFindingId()}}, nil
}

func TestVerdictUpdate_BodyFindingIDOverriddenByPath(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &verdictPinBackend{srv: grpc.NewServer()}
	pb.RegisterResultServiceServer(b.srv, b)
	pb.RegisterReportServiceServer(b.srv, b)
	pb.RegisterTaskServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)

	tr := NewTranscoder(BackendAddrs{ResultAddr: lis.Addr().String(), TaskAddr: lis.Addr().String(), CallTimeoutS: 5})
	defer tr.Close()

	// owner 身份过归属门禁；body 指向他人 finding（f-victim）——路径段必须赢
	code, out := httpJSONWithClaims(t, tr, "", "user-a", http.MethodPut, "/v1/findings/f-legit/verdict",
		`{"finding_id":"f-victim","verdict":"AI_VERDICT_FALSE_POSITIVE"}`)
	if code != 200 {
		t.Fatalf("verdict update: %d %v", code, out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.got == nil || b.got.GetFindingId() != "f-legit" {
		t.Fatalf("后端实收 finding_id=%q（须为路径段 f-legit，body f-victim 是越权企图）", b.got.GetFindingId())
	}
}
