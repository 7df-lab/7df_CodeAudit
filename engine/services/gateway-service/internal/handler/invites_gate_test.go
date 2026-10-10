package handler

// V2.2 (ADR-255): /v1/invites 四路由 admin 门禁 + /v1/auth/registration-policy 公开面。
// 门禁语义沿 users_gate_test（R73）同型：非 admin 403 且后端不触达；admin 到达后端。

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// inviteGateBackend — 进程内真实 gRPC 后端：捕获邀请码管理调用、应答罐装记录。
type inviteGateBackend struct {
	pb.UnimplementedUserServiceServer
	addr string
	srv  *grpc.Server

	mu        sync.Mutex
	calls     []string
	mode      string
	createdBy string
}

func startInviteGateBackend(t *testing.T, mode string) *inviteGateBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &inviteGateBackend{addr: lis.Addr().String(), srv: grpc.NewServer(), mode: mode}
	pb.RegisterUserServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *inviteGateBackend) record(call string) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	b.mu.Unlock()
}

func (b *inviteGateBackend) called(call string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (b *inviteGateBackend) CreateInviteCode(ctx context.Context, req *pb.CreateInviteCodeRequest) (*pb.InviteCode, error) {
	b.record("Create")
	b.mu.Lock()
	b.createdBy = req.GetCreatedBy()
	b.mu.Unlock()
	return &pb.InviteCode{Id: "invite-1", Code: "CODEAUDIT-AAAAA-BBBBB", State: "active"}, nil
}

func (b *inviteGateBackend) ListInviteCodes(ctx context.Context, req *pb.ListInviteCodesRequest) (*pb.ListInviteCodesResponse, error) {
	b.record("List")
	return &pb.ListInviteCodesResponse{ConfigCodes: []string{"STATIC-1"}}, nil
}

func (b *inviteGateBackend) RevokeInviteCode(ctx context.Context, req *pb.RevokeInviteCodeRequest) (*pb.InviteCode, error) {
	b.record("Revoke")
	return &pb.InviteCode{Id: req.GetId(), State: "revoked"}, nil
}

func (b *inviteGateBackend) DeleteInviteCode(ctx context.Context, req *pb.DeleteInviteCodeRequest) (*emptypb.Empty, error) {
	b.record("Delete")
	return &emptypb.Empty{}, nil
}

func (b *inviteGateBackend) GetRegistrationPolicy(ctx context.Context, req *pb.GetRegistrationPolicyRequest) (*pb.GetRegistrationPolicyResponse, error) {
	b.record("Policy")
	return &pb.GetRegistrationPolicyResponse{Mode: b.mode}, nil
}

// 主锁：invites 四路由 developer 403 且后端零触达；admin 全通。
func TestInvites_AdminGate(t *testing.T) {
	b := startInviteGateBackend(t, "invitation")
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	routes := []struct{ method, path, body, call string }{
		{http.MethodGet, "/v1/invites", "", "List"},
		{http.MethodPost, "/v1/invites", `{"note":"x"}`, "Create"},
		{http.MethodPost, "/v1/invites/invite-1/revoke", "", "Revoke"},
		{http.MethodDelete, "/v1/invites/invite-1", "", "Delete"},
	}
	for _, rt := range routes {
		code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-1", rt.method, rt.path, rt.body)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s developer: got %d, want 403", rt.method, rt.path, code)
		}
		if b.called(rt.call) {
			t.Fatalf("%s %s developer 被拒后仍触达后端", rt.method, rt.path)
		}
	}
	for _, rt := range routes {
		code, _ := httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", rt.method, rt.path, rt.body)
		if code != http.StatusOK {
			t.Fatalf("%s %s admin: got %d, want 200", rt.method, rt.path, code)
		}
		if !b.called(rt.call) {
			t.Fatalf("%s %s admin 未触达后端", rt.method, rt.path)
		}
	}
	// 审查批：created_by=JWT sub 注入（admin-1），不再服务端恒 "admin"
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createdBy != "admin-1" {
		t.Fatalf("created_by 注入: got %q, want admin-1 (JWT sub)", b.createdBy)
	}
}

// 公开面（Q10）：registration-policy 免认证可达（转码器直连，无 JWT 注入），
// mode 透传且不泄露码表。
func TestRegistrationPolicy_PublicPassthrough(t *testing.T) {
	b := startInviteGateBackend(t, "open")
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/auth/registration-policy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"mode":"open"`) {
		t.Fatalf("mode 透传失败: %s", string(body))
	}
	if strings.Contains(string(body), "STATIC") || strings.Contains(string(body), "CODEAUDIT") {
		t.Fatalf("policy 面泄露码表: %s", string(body))
	}
	if !b.called("Policy") {
		t.Fatal("后端未触达 GetRegistrationPolicy")
	}
}
