package service

// ADR-217 服务层测试：R4 幂等键强制、manager HTTP 状态码 → gRPC 映射、
// 凭据只进不出（响应不回流）。manager 用 httptest 假体（env 覆盖 OPENSHELL_MANAGER_URL）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newInferenceSvc(t *testing.T, h http.HandlerFunc) *RuntimeServiceImpl {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("OPENSHELL_MANAGER_URL", srv.URL)
	t.Setenv("OPENSHELL_MANAGER_TOKEN", "")
	t.Setenv("OPENSHELL_MANAGER_CONFIG", "")
	return NewRuntimeService()
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
		Credentials: map[string]string{"API_KEY": "sk-secret"}, // R55 后夹具须用约定键
	})
	if err != nil || resp.GetName() != "prov-b" || !resp.GetCreated() {
		t.Fatalf("upsert: %v %+v", err, resp)
	}
	// 响应体不含凭据（UpsertInferenceProviderResponse 只有 name/created——结构即纪律）
	if resp.String() == "sk-secret" {
		t.Fatalf("credentials leaked in response")
	}
}

// R55: 别名/死键必须 InvalidArgument——
// 网关（闭源件）按类型认约定键，别名键静默存储不被识别，切路由验证时才失败。
// 反转点：旧 anthropic 约定键 BASE_URL 被网关 b448ef29 无视（死键）→ 从放行表进拦截表，
// 指路 ANTHROPIC_BASE_URL；DEEPSEEK_* 随 deepseek 型除名同拦。
func TestInference_UpsertRejectsLowercaseAliasKeys(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		t.Fatal("alias keys must be rejected before reaching manager")
	})
	cases := []struct {
		name       string
		typ        string
		config     map[string]string
		credential map[string]string
		wantHint   string // 指路键须出现在错误里
	}{
		{"openai base_url", "openai", map[string]string{"base_url": "https://x"}, nil, "OPENAI_BASE_URL"},
		{"openai api_key", "openai", nil, map[string]string{"api_key": "sk"}, "OPENAI_API_KEY"},
		{"anthropic base_url", "anthropic", map[string]string{"baseUrl": "https://x"}, nil, "ANTHROPIC_BASE_URL"},
		{"anthropic 旧约定键 BASE_URL 死键反转", "anthropic", map[string]string{"BASE_URL": "https://x"}, nil, "ANTHROPIC_BASE_URL"},
		{"openai 误填 BASE_URL", "openai", map[string]string{"BASE_URL": "https://x"}, nil, "OPENAI_BASE_URL"},
		{"deepseek 型死键反转", "deepseek", map[string]string{"DEEPSEEK_BASE_URL": "https://x"}, nil, "OPENAI_BASE_URL"},
	}
	for _, c := range cases {
		_, err := svc.UpsertInferenceProvider(context.Background(),
			&pb.UpsertInferenceProviderRequest{
				Metadata: &pb.RequestMetadata{RequestId: "r-alias"},
				Name:     "prov-alias", Type: c.typ,
				Config: c.config, Credentials: c.credential,
			})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: want InvalidArgument, got %v", c.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), c.wantHint) {
			t.Fatalf("%s: error should point to canonical key %q, got %v", c.name, c.wantHint, err)
		}
	}
	// 新约定键与自定义键照常放行（独立 200 假体）：anthropic=ANTHROPIC_BASE_URL/API_KEY
	svcOK := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "prov-ok", "created": true})
	})
	_, err := svcOK.UpsertInferenceProvider(context.Background(),
		&pb.UpsertInferenceProviderRequest{
			Metadata: &pb.RequestMetadata{RequestId: "r-canonical"},
			Name:     "prov-ok", Type: "anthropic",
			Config:      map[string]string{"ANTHROPIC_BASE_URL": "https://x", "custom_env": "v"},
			Credentials: map[string]string{"API_KEY": "sk"},
		})
	if err != nil {
		t.Fatalf("canonical keys must pass: %v", err)
	}
}

// R55 补遗：约定键的全小写形态（openai_base_url/anthropic_base_url 等）同样拦截。
func TestInference_UpsertRejectsLowercaseCanonicalForms(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		t.Fatal("lowercase canonical-form keys must be rejected before reaching manager")
	})
	for _, c := range []struct {
		name string
		typ  string
		kv   map[string]string
	}{
		{"openai_base_url", "openai", map[string]string{"openai_base_url": "https://x"}},
		{"openai_api_key", "openai", map[string]string{"openai_api_key": "sk"}},
		{"deepseek_base_url", "openai", map[string]string{"deepseek_base_url": "https://x"}},
		{"anthropic_base_url（2026-09-17 新约定键小写形态）", "anthropic", map[string]string{"anthropic_base_url": "https://x"}},
		{"anthropic_api_key", "anthropic", map[string]string{"anthropic_api_key": "sk"}},
	} {
		_, err := svc.UpsertInferenceProvider(context.Background(),
			&pb.UpsertInferenceProviderRequest{
				Metadata: &pb.RequestMetadata{RequestId: "r-lc-" + c.name},
				Name:     "prov-lc", Type: c.typ, Config: c.kv,
			})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: want InvalidArgument, got %v", c.name, err)
		}
	}
}

// 切片3b：ListWorkspaces——透传契约（limit/offset→manager 查询参数）+ 负值钳制。
func TestListWorkspaces_HappyPath(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces" {
			t.Errorf("manager path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "7" || r.URL.Query().Get("offset") != "3" {
			t.Errorf("manager query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"workspaces":[{"name":"default","phase":"Active","labels":{}},{"name":"team-a","phase":"Active","labels":{"team":"a"}}]}`))
	})
	resp, err := svc.ListWorkspaces(context.Background(), &pb.ListWorkspacesRequest{Limit: 7, Offset: 3})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(resp.Workspaces) != 2 || resp.Workspaces[1].Name != "team-a" ||
		resp.Workspaces[1].Labels["team"] != "a" {
		t.Fatalf("workspaces = %v", resp.Workspaces)
	}
}

func TestListWorkspaces_NegativePagingInvalidArgument(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("negative paging must not reach manager")
	})
	for _, req := range []*pb.ListWorkspacesRequest{{Limit: -1}, {Offset: -2}} {
		if _, err := svc.ListWorkspaces(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("req=%v: want InvalidArgument, got %v", req, err)
		}
	}
}

// 切片3b 扩面：建/删区——R4 幂等键 + manager 409→FailedPrecondition（details 原文）。
func TestWorkspaceCreate_RequiresRequestID(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("无幂等键不得触达 manager")
	})
	if _, err := svc.CreateWorkspace(context.Background(), &pb.CreateWorkspaceRequest{Name: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestWorkspaceCreateDelete_Passthrough(t *testing.T) {
	var gotPath, gotMethod string
	svc := newInferenceSvc(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if r.Method == "POST" {
			w.Write([]byte(`{"name":"team-b","phase":"WORKSPACE_PHASE_ACTIVE","labels":{"a":"b"}}`))
		} else {
			w.Write([]byte(`{"name":"team-b","deleted":true}`))
		}
	})
	w, err := svc.CreateWorkspace(context.Background(), &pb.CreateWorkspaceRequest{
		Metadata: &pb.RequestMetadata{RequestId: "r1"}, Name: "team-b", Labels: map[string]string{"a": "b"}})
	if err != nil || w.Name != "team-b" || w.Labels["a"] != "b" {
		t.Fatalf("create = %v, %v", w, err)
	}
	if gotPath != "/api/v1/workspaces" || gotMethod != "POST" {
		t.Fatalf("manager call = %s %s", gotMethod, gotPath)
	}
	d, err := svc.DeleteWorkspace(context.Background(), &pb.DeleteWorkspaceRequest{
		Metadata: &pb.RequestMetadata{RequestId: "r2"}, Name: "team-b"})
	if err != nil || !d.Deleted {
		t.Fatalf("delete = %v, %v", d, err)
	}
}

func TestWorkspaceDelete_ResourceGuard409(t *testing.T) {
	svc := newInferenceSvc(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"workspace 'x' still contains resources: sandbox, service"}`))
	})
	_, err := svc.DeleteWorkspace(context.Background(), &pb.DeleteWorkspaceRequest{
		Metadata: &pb.RequestMetadata{RequestId: "r3"}, Name: "x"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "still contains resources") {
		t.Fatalf("want FailedPrecondition+details, got %v", err)
	}
}
