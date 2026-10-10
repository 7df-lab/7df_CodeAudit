package handler

// TP20-T2 切换 Agent 收口：GET /v1/agents 登录可读转发 + POST /v1/tasks/{id}/agent
// 动作路由（归属门禁与 pause 同面=canAccessTask 统一面，admin 旁路在此仅作通道测）。

import (
	"context"
	"net"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

// agentsBackend — 假 runtime：ListAgents 固定两档（首条缺省）。
type agentsBackend struct {
	pb.UnimplementedRuntimeServiceServer
	addr string
}

func (b *agentsBackend) ListAgents(context.Context, *pb.ListAgentsRequest) (*pb.ListAgentsResponse, error) {
	return &pb.ListAgentsResponse{Agents: []*pb.AgentProviderInfo{
		{Id: "dsh", Dialect: "dsh-sse", IsDefault: true,
			ToolEvents: true, StructuredSubmit: true, ConcludeSemantics: true},
		{Id: "maka", Dialect: "maka-runtime-host", Interaction: false},
	}}, nil
}

func startAgentsBackend(t *testing.T) *agentsBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &agentsBackend{addr: lis.Addr().String()}
	srv := grpc.NewServer()
	pb.RegisterRuntimeServiceServer(srv, b)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return b
}

func TestAgents_LoggedInReadablePassthrough(t *testing.T) {
	b := startAgentsBackend(t)
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	// 普通登录用户（非 admin）：清单=创建页/切换 Modal 同源，登录可读（计划 §0）
	code, out := httpJSON(t, tr, "GET", "/v1/agents", "")
	if code != 200 {
		t.Fatalf("GET /v1/agents: want 200, got %d %v", code, out)
	}
	agents, _ := out["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents = %v", out)
	}
	first, _ := agents[0].(map[string]any)
	if first["id"] != "dsh" || first["is_default"] != true {
		t.Fatalf("首条应 dsh+is_default: %v", first)
	}
}

func TestAgents_MethodGuard(t *testing.T) {
	b := startAgentsBackend(t)
	tr := NewTranscoder(BackendAddrs{RuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	if code, _ := httpJSON(t, tr, "POST", "/v1/agents", "{}"); code != 404 {
		t.Fatalf("POST /v1/agents 应 404, got %d", code)
	}
}

// setAgentBackend — 假 task 服务：捕获 SetTaskAgent 请求。
type setAgentBackend struct {
	pb.UnimplementedTaskServiceServer
	addr string
	mu   sync.Mutex
	got  *pb.SetTaskAgentRequest
}

func (b *setAgentBackend) SetTaskAgent(_ context.Context, req *pb.SetTaskAgentRequest) (*pb.ScanTask, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = req
	return &pb.ScanTask{TaskId: req.GetTaskId(), Agent: req.GetAgent(), Status: pb.TaskStatus_TASK_STATUS_PAUSED}, nil
}

func TestTaskAgentAction_RoutesToSetTaskAgent(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &setAgentBackend{addr: lis.Addr().String()}
	srv := grpc.NewServer()
	pb.RegisterTaskServiceServer(srv, b)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	tr := NewTranscoder(BackendAddrs{TaskAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	// admin 旁路 canAccessTask（归属门禁语义面已由 authz_test 族锁定）
	code, out := httpJSONAsAdmin(t, tr, "POST", "/v1/tasks/t-sw1/agent", `{"task_id":"evil","agent":"maka"}`)
	if code != 200 {
		t.Fatalf("POST /v1/tasks/t-sw1/agent: %d %v", code, out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.got == nil || b.got.GetAgent() != "maka" {
		t.Fatalf("agent 未路由: %+v", b.got)
	}
	if b.got.GetTaskId() != "t-sw1" {
		t.Fatalf("R89 路径身份应权威（body task_id 不采信）: %q", b.got.GetTaskId())
	}
	if out["agent"] != "maka" {
		t.Fatalf("响应应回显换笔结果: %v", out)
	}
}
