package handler

// 切片3b 锁定测试：/v1/workspaces——
//   ① admin 门禁（与 /v1/inference 同款 requireAdmin）；
//   ② REST→gRPC 透传（limit/offset 查询参数 → proto 字段；响应 protojson）；
//   ③ 非法 limit/offset → 400（网关层钳制，不触达后端）。
// 后端为进程内真实 gRPC（捕获实收请求），与 inference_test.go 同型。

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workspacesBackend — 只实现 ListWorkspaces 的最小捕获后端。
type workspacesBackend struct {
	pb.UnimplementedRuntimeServiceServer
	addr          string
	mu            sync.Mutex
	limit, offset int32
	createOK      bool
	createMD      string
	deleteErr     string
}

func (b *workspacesBackend) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.WorkspaceInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createMD = req.GetMetadata().GetRequestId()
	if !b.createOK {
		return nil, status.Error(codes.FailedPrecondition, "workspace already exists")
	}
	return &pb.WorkspaceInfo{Name: req.GetName(), Phase: "Active", Labels: req.GetLabels()}, nil
}

func (b *workspacesBackend) DeleteWorkspace(ctx context.Context, req *pb.DeleteWorkspaceRequest) (*pb.DeleteWorkspaceResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != "" {
		return nil, status.Error(codes.FailedPrecondition, b.deleteErr)
	}
	return &pb.DeleteWorkspaceResponse{Name: req.GetName(), Deleted: true}, nil
}

func (b *workspacesBackend) ListWorkspaces(ctx context.Context, req *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	b.mu.Lock()
	b.limit, b.offset = req.GetLimit(), req.GetOffset()
	b.mu.Unlock()
	return &pb.ListWorkspacesResponse{Workspaces: []*pb.WorkspaceInfo{
		{Name: "default", Phase: "Active"},
		{Name: "team-a", Phase: "Active", Labels: map[string]string{"team": "a"}},
	}}, nil
}

func startWorkspacesBackend(t *testing.T) *workspacesBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &workspacesBackend{addr: lis.Addr().String()}
	srv := grpc.NewServer()
	pb.RegisterRuntimeServiceServer(srv, b)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return b
}

func TestWorkspaces_AdminGate(t *testing.T) {
	b := startWorkspacesBackend(t)
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	if code, _ := httpJSON(t, tr, "GET", "/v1/workspaces", ""); code != http.StatusForbidden {
		t.Fatalf("GET /v1/workspaces without admin: want 403, got %d", code)
	}
}

func TestWorkspaces_PassthroughLimitOffset(t *testing.T) {
	b := startWorkspacesBackend(t)
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, out := httpJSONAsAdmin(t, tr, "GET", "/v1/workspaces?limit=5&offset=1", "")
	if code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", code)
	}
	wss, _ := out["workspaces"].([]any)
	if len(wss) != 2 {
		t.Fatalf("workspaces = %v", out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit != 5 || b.offset != 1 {
		t.Fatalf("backend captured limit=%d offset=%d, want 5/1", b.limit, b.offset)
	}
}

func TestWorkspaces_BadLimit400(t *testing.T) {
	b := startWorkspacesBackend(t)
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	for _, q := range []string{"limit=abc", "limit=-1", "offset=-2"} {
		code, out := httpJSONAsAdmin(t, tr, "GET", "/v1/workspaces?"+q, "")
		if code != http.StatusBadRequest {
			t.Fatalf("GET /v1/workspaces?%s: want 400, got %d (%v)", q, code, out)
		}
	}
}

// 切片3b 扩面：建/删区写面——路径名权威 + 幂等键注入 + 409 映射。
func TestWorkspaces_CreateDelete(t *testing.T) {
	b := startWorkspacesBackend(t)
	b.createOK = true
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, out := httpJSONAsAdmin(t, tr, "POST", "/v1/workspaces",
		`{"name":"team-b","labels":{"team":"b"}}`)
	if code != http.StatusOK {
		t.Fatalf("create: want 200, got %d (%v)", code, out)
	}
	if out["name"] != "team-b" {
		t.Fatalf("create resp = %v", out)
	}
	b.mu.Lock()
	hasMD := b.createMD != ""
	b.mu.Unlock()
	if !hasMD {
		t.Fatal("幂等键未注入（R4）")
	}

	code, _ = httpJSONAsAdmin(t, tr, "DELETE", "/v1/workspaces/team-b", "")
	if code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d", code)
	}
}

func TestWorkspaces_DeleteConflict409(t *testing.T) {
	b := startWorkspacesBackend(t)
	b.deleteErr = "workspace 'x' still contains resources: sandbox"
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, out := httpJSONAsAdmin(t, tr, "DELETE", "/v1/workspaces/x", "")
	if code != http.StatusConflict {
		t.Fatalf("want 409, got %d (%v)", code, out)
	}
}
