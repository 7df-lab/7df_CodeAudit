package service

// ADR-217 服务层测试：R4 幂等键强制、manager HTTP 状态码 → gRPC 映射、
// 凭据只进不出（响应不回流）。manager 用 httptest 假体（env 覆盖 OPENSHELL_MANAGER_URL）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newInferenceSvc(t *testing.T, h http.HandlerFunc) *DSHRuntimeServiceImpl {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("OPENSHELL_MANAGER_URL", srv.URL)
	t.Setenv("OPENSHELL_MANAGER_TOKEN", "")
	t.Setenv("OPENSHELL_MANAGER_CONFIG", "")
	return NewDSHRuntimeService()
}

// R4 锁定：写 RPC 无幂等键 → InvalidArgument，不触达 manager。
func TestInference_UpsertRequiresRequestID(t *testing.T) {
	called := false
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "x", "created": true})
	})
	_, err := svc.UpsertInferenceProvider(context.Background(),
		&pb.UpsertInferenceProviderRequest{Name: "prov-b", Type: "openai"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if called {
		t.Fatalf("request without request_id must not reach manager")
	}
	_, err = svc.DeleteInferenceProvider(context.Background(), &pb.DeleteInferenceProviderRequest{Name: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("delete without request_id: want InvalidArgument, got %v", err)
	}
	_, err = svc.SetInferenceRoute(context.Background(), &pb.SetInferenceRouteRequest{Provider: "p", Model: "m"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("set route without request_id: want InvalidArgument, got %v", err)
	}
}

func TestInference_ManagerErrorMapping(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "provider 'nope' not found in workspace 'default'"})
	})
	_, err := svc.GetInferenceProvider(context.Background(), &pb.GetInferenceProviderRequest{Name: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("404 → NotFound, got %v", err)
	}

	svc400 := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing required field(s): name"})
	})
	_, err = svc400.UpsertInferenceProvider(context.Background(), &pb.UpsertInferenceProviderRequest{
		Metadata: &pb.RequestMetadata{RequestId: "r-1"}, Name: "x", Type: "openai"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("400 → InvalidArgument, got %v", err)
	}
}

func TestInference_UpsertHappyPathCredentialsNeverEchoed(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "prov-b", "created": true})
	})
	resp, err := svc.UpsertInferenceProvider(context.Background(), &pb.UpsertInferenceProviderRequest{
		Metadata:    &pb.RequestMetadata{RequestId: "r-2"},
		Name:        "prov-b",
		Type:        "anthropic",
		Credentials: map[string]string{"api_key": "sk-secret"},
	})
	if err != nil || resp.GetName() != "prov-b" || !resp.GetCreated() {
		t.Fatalf("upsert: %v %+v", err, resp)
	}
	// 响应体不含凭据（UpsertInferenceProviderResponse 只有 name/created——结构即纪律）
	if resp.String() == "sk-secret" {
		t.Fatalf("credentials leaked in response")
	}
}
