package handler

// ADR-217 锁定测试：/v1/inference/* 管理面——
//   ① 全路由 admin 门禁（无 role claim → 403，与 /v1/users 管理端同款 requireAdmin）；
//   ② REST→gRPC 透传契约（路径名权威、写路由幂等键网关注入、响应 protojson snake_case）。
// 后端为进程内真实 gRPC（捕获实收请求），不 mock HTTP 层。

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
	"google.golang.org/grpc"
)

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// inferenceCaptureBackend — 捕获 6 RPC 实收请求的进程内后端。
type inferenceCaptureBackend struct {
	pb.UnimplementedDSHRuntimeServiceServer
	addr string
	srv  *grpc.Server
	mu   sync.Mutex

	upsertName  string
	upsertCreds map[string]string
	upsertHasMD bool
	deleteName  string
	routeSet    *pb.SetInferenceRouteRequest
}

func startInferenceBackend(t *testing.T) *inferenceCaptureBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &inferenceCaptureBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterDSHRuntimeServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *inferenceCaptureBackend) ListInferenceProviders(ctx context.Context, _ *pb.ListInferenceProvidersRequest) (*pb.ListInferenceProvidersResponse, error) {
	return &pb.ListInferenceProvidersResponse{Providers: []*pb.InferenceProviderInfo{{
		Name: "prov-a", Type: "openai", Config: map[string]string{"base_url": "https://x/v1"},
	}}}, nil
}

func (b *inferenceCaptureBackend) UpsertInferenceProvider(ctx context.Context, req *pb.UpsertInferenceProviderRequest) (*pb.UpsertInferenceProviderResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.upsertName = req.GetName()
	b.upsertCreds = req.GetCredentials()
	b.upsertHasMD = req.GetMetadata().GetRequestId() != ""
	return &pb.UpsertInferenceProviderResponse{Name: req.GetName(), Created: true}, nil
}

func (b *inferenceCaptureBackend) DeleteInferenceProvider(ctx context.Context, req *pb.DeleteInferenceProviderRequest) (*pb.DeleteInferenceProviderResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteName = req.GetName()
	return &pb.DeleteInferenceProviderResponse{Deleted: true}, nil
}

func (b *inferenceCaptureBackend) GetInferenceRoute(ctx context.Context, _ *pb.GetInferenceRouteRequest) (*pb.InferenceRouteInfo, error) {
	return &pb.InferenceRouteInfo{Provider: "prov-a", Model: "m-1", Version: 4}, nil
}

func (b *inferenceCaptureBackend) SetInferenceRoute(ctx context.Context, req *pb.SetInferenceRouteRequest) (*pb.SetInferenceRouteResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routeSet = req
	return &pb.SetInferenceRouteResponse{
		Provider: req.GetProvider(), Model: req.GetModel(), Version: 5,
		ValidationPerformed: true,
		ValidatedEndpoints:  []*pb.ValidatedEndpoint{{Url: "https://gw/v1", Protocol: "https"}},
	}, nil
}

// httpJSONAsAdmin — 注入 ROLE_ADMIN 后走转码器（生产链路由 JWTMiddleware 写入 claim）。
func httpJSONAsAdmin(t *testing.T, tr *Transcoder, method, path, body string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.UserRoleKey, "ROLE_ADMIN")
		tr.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, srv.URL+path, nil)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decodeJSONBody(t, resp)
}

// TestInference_AdminGate — 无 admin role 的任何 inference 路由 → 403（不触达后端）。
func TestInference_AdminGate(t *testing.T) {
	b := startInferenceBackend(t)
	tr := NewTranscoder(BackendAddrs{DSHRuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	for _, path := range []string{"/v1/inference/providers", "/v1/inference/route"} {
		code, _ := httpJSON(t, tr, "GET", path, "")
		if code != http.StatusForbidden {
			t.Fatalf("GET %s without admin: want 403, got %d", path, code)
		}
	}
	code, _ := httpJSON(t, tr, "PUT", "/v1/inference/route", `{"provider":"p","model":"m"}`)
	if code != http.StatusForbidden {
		t.Fatalf("PUT route without admin: want 403, got %d", code)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.routeSet != nil {
		t.Fatalf("non-admin request reached backend (gate bypassed)")
	}
}

// TestInference_ProviderCRUDAndRoute — 透传契约：路径名权威、幂等键注入、
// 凭据透传、验证回执字段、未知子路径 404。
func TestInference_ProviderCRUDAndRoute(t *testing.T) {
	b := startInferenceBackend(t)
	tr := NewTranscoder(BackendAddrs{DSHRuntimeAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, out := httpJSONAsAdmin(t, tr, "GET", "/v1/inference/providers", "")
	if code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", code)
	}
	provs, _ := out["providers"].([]any)
	if len(provs) != 1 {
		t.Fatalf("list providers = %v", out)
	}

	code, out = httpJSONAsAdmin(t, tr, "POST", "/v1/inference/providers",
		`{"name":"prov-b","type":"anthropic","credentials":{"api_key":"sk-x"},"config":{"base_url":"https://y/v1"}}`)
	if code != http.StatusOK || out["created"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	b.mu.Lock()
	if b.upsertName != "prov-b" || b.upsertCreds["api_key"] != "sk-x" || !b.upsertHasMD {
		b.mu.Unlock()
		t.Fatalf("upsert capture: name=%q creds=%v hasMD=%v", b.upsertName, b.upsertCreds, b.upsertHasMD)
	}
	b.mu.Unlock()

	// PUT providers/{name}：路径名权威（body 携带不同 name 也必须被路径覆盖）
	code, _ = httpJSONAsAdmin(t, tr, "PUT", "/v1/inference/providers/prov-b",
		`{"name":"prov-OTHER","type":"anthropic"}`)
	if code != http.StatusOK {
		t.Fatalf("update: want 200, got %d", code)
	}
	b.mu.Lock()
	if b.upsertName != "prov-b" {
		b.mu.Unlock()
		t.Fatalf("path name must win: got %q", b.upsertName)
	}
	b.mu.Unlock()

	code, _ = httpJSONAsAdmin(t, tr, "DELETE", "/v1/inference/providers/prov-b", "")
	if code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d", code)
	}
	b.mu.Lock()
	if b.deleteName != "prov-b" {
		b.mu.Unlock()
		t.Fatalf("delete capture: %q", b.deleteName)
	}
	b.mu.Unlock()

	code, out = httpJSONAsAdmin(t, tr, "GET", "/v1/inference/route", "")
	if code != http.StatusOK || out["provider"] != "prov-a" || out["version"] != "4" {
		t.Fatalf("get route: %d %v（version 为 uint64，protojson 序列化为 string）", code, out)
	}

	code, out = httpJSONAsAdmin(t, tr, "PUT", "/v1/inference/route",
		`{"provider":"prov-a","model":"glm-5.3-flash","no_verify":false}`)
	if code != http.StatusOK {
		t.Fatalf("set route: want 200, got %d", code)
	}
	if out["validation_performed"] != true {
		t.Fatalf("set route response missing validation receipt: %v", out)
	}
	eps, _ := out["validated_endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("validated_endpoints = %v", out)
	}

	code, _ = httpJSONAsAdmin(t, tr, "GET", "/v1/inference/nope", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown subpath: want 404, got %d", code)
	}
}
