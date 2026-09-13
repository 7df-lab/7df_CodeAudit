package orchestrator

// R60继承部分失败必须令编排失败走重试链——FailedCount 被吞没
// 时缺继承行的任务照常 COMPLETED，违背"完整性优先"（incremental.go:52 注释）。

import (
	"context"
	"net"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeResultService — 只实现 InheritFindings 的进程内 gRPC 假体。
type fakeResultService struct {
	pb.UnimplementedResultServiceServer
	failedCount    int32
	inheritedCount int32
}

func (f *fakeResultService) InheritFindings(ctx context.Context, req *pb.InheritFindingsRequest) (*pb.InheritFindingsResponse, error) {
	if req.GetBaselineTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "baseline required")
	}
	return &pb.InheritFindingsResponse{
		InheritedCount: f.inheritedCount, SkippedCount: 0, FailedCount: f.failedCount,
	}, nil
}

func startFakeResult(t *testing.T, f *fakeResultService) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterResultServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestRunIncrementalInherit_PartialFailureFails(t *testing.T) {
	addr := startFakeResult(t, &fakeResultService{failedCount: 1, inheritedCount: 2})
	o := New(Config{ResultAddr: addr})
	inc := &IncrementalContext{}
	inc.Set(true, "base-task", []string{"mod.py"}, nil, "")
	stageCalls := []string{}
	stage := func(k, m string) { stageCalls = append(stageCalls, k+":"+m) }

	err := o.runIncrementalInherit(context.Background(), RunRequest{TaskID: "t-r60", Incremental: inc}, stage)
	if err == nil {
		t.Fatal("partial inherit failure (FailedCount=1) must fail orchestration (R60)")
	}
	if !strings.Contains(err.Error(), "FailedCount") && !strings.Contains(err.Error(), "继承") {
		t.Fatalf("error should name the failure count, got: %v", err)
	}
}

func TestRunIncrementalInherit_AllInheritedSucceeds(t *testing.T) {
	addr := startFakeResult(t, &fakeResultService{failedCount: 0, inheritedCount: 3})
	o := New(Config{ResultAddr: addr})
	inc := &IncrementalContext{}
	inc.Set(true, "base-task", []string{"mod.py"}, nil, "")
	if err := o.runIncrementalInherit(context.Background(), RunRequest{TaskID: "t-r60b", Incremental: inc},
		func(k, m string) {}); err != nil {
		t.Fatalf("zero FailedCount must succeed, got %v", err)
	}
}
