package handler

// R81 锁定测试（内部包：需触及未导出 idempotency/toolCommands）：
// ① 落盘失败不进幂等缓存——此前成功响应照常缓存，同 request_id 重试永远拿缓存，
//    本批 findings 对下游永久丢失；修复后重试重走扫描+落盘（result 侧
//    (request_id, finding_id) 幂等保证不重复落盘）。
// ② bumpIdemGuard 并发原子性（原裸 idemCount++ 为数据竞争，R69 引入）。

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeIdemResult — 可切换成败的 result-service 假后端（计数 BatchCreateFindings 次数）。
type fakeIdemResult struct {
	pb.UnimplementedResultServiceServer
	fail    atomic.Bool
	calls   atomic.Int32
	addrs   string
	listener net.Listener
	srv     *grpc.Server
}

func startFakeIdemResult(t *testing.T, fail bool) *fakeIdemResult {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeIdemResult{listener: lis, srv: grpc.NewServer()}
	f.fail.Store(fail)
	pb.RegisterResultServiceServer(f.srv, f)
	go func() { _ = f.srv.Serve(lis) }()
	t.Cleanup(f.srv.Stop)
	return f
}

func (f *fakeIdemResult) addr() string { return f.listener.Addr().String() }

func (f *fakeIdemResult) BatchCreateFindings(ctx context.Context, req *pb.BatchCreateFindingsRequest) (*pb.BatchCreateFindingsResponse, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return nil, status.Error(codes.Unavailable, "fake result down")
	}
	ids := make([]string, 0, len(req.GetFindings()))
	for i := range req.GetFindings() {
		ids = append(ids, fmt.Sprintf("f-%d", i))
	}
	return &pb.BatchCreateFindingsResponse{FindingIds: ids}, nil
}


// idemCountAsInt — M56 变异两态（atomic.Int64 / 裸 int）下的断言读取。
func idemCountAsInt(v any) int64 {
	switch x := v.(type) {
	case atomic.Int64:
		return x.Load()
	case int:
		return int64(x)
	}
	return -1
}

const r81BanditJSON = `{"results":[{"test_id":"B101","test_name":"hardcoded","filename":"app.py","line_number":1,"issue_text":"x","issue_severity":"HIGH","issue_confidence":"MEDIUM"}]}`

func r81Handler(t *testing.T, result *fakeIdemResult) *SASTAdapterHandler {
	t.Helper()
	h := NewSASTAdapterHandler(result.addr())
	// 覆写 bandit 工具执行映射为 echo 假件（解析器按工具 id 取，bandit 解析器真实复用）
	h.toolCommands["bandit"] = toolCommand{
		argv:      []string{"echo", r81BanditJSON},
		rawFormat: "bandit",
	}
	return h
}

func TestRunSASTScan_PersistFailureNotCached(t *testing.T) {
	if _, err := exec.LookPath("echo"); err != nil {
		t.Skip("echo not available")
	}
	result := startFakeIdemResult(t, true) // 先落盘必败
	h := r81Handler(t, result)
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &pb.RunSASTScanRequest{
		Metadata:    &pb.RequestMetadata{RequestId: "r81-1"},
		TaskId:      "t-r81",
		ProjectPath: proj,
		ToolId:      "bandit",
	}

	// 第一次：扫描成功、落盘失败 → 响应不得进幂等缓存
	if _, err := h.RunSASTScan(context.Background(), req); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, ok := h.idempotency.Load("r81-1"); ok {
		t.Fatal("落盘失败仍写幂等缓存——同键重试永远拿缓存，findings 对下游永久丢失（R81）")
	}

	// 第二次（同 request_id）：必须重走扫描+落盘（此时落盘恢复 → 成功并缓存）
	result.fail.Store(false)
	if _, err := h.RunSASTScan(context.Background(), req); err != nil {
		t.Fatalf("retry scan: %v", err)
	}
	if _, ok := h.idempotency.Load("r81-1"); !ok {
		t.Fatal("落盘恢复后的成功重试未进缓存（幂等重放面丢失）")
	}

	// 第三次：幂等重放——不再触达落盘
	if _, err := h.RunSASTScan(context.Background(), req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := result.calls.Load(); got != 2 {
		t.Fatalf("BatchCreateFindings 调用 %d 次, want 2（重放不应再落盘）", got)
	}
}

// R81: 容量护栏计数并发原子（原裸 int++ 丢失更新；M56 以 -race 判杀）。
func TestBumpIdemGuard_ConcurrentAtomic(t *testing.T) {
	h := &SASTAdapterHandler{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.bumpIdemGuard()
			}
		}()
	}
	wg.Wait()
	// 类型开关同时兼容 atomic.Int64 与 M56 变异后的裸 int（两态编译+两态断言）
	if got := idemCountAsInt(h.idemCount); got != 1600 {
		t.Fatalf("idemCount = %d, want 1600（计数丢失更新=非原子）", got)
	}
}
