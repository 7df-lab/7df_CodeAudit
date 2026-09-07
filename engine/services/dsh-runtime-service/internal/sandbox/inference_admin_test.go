package sandbox

// ADR-217 传输层测试：inference 管理面对 manager /api/v1/inference/* 的调用
// 契约（workspace 注入、凭据透传、类型化 HTTP 错误、验证回执解析）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func newInferenceRunner(t *testing.T, cfg Config, h http.HandlerFunc) *ManagerRunner {
	t.Helper()
	t.Setenv("OPENSHELL_MANAGER_URL", "")
	t.Setenv("OPENSHELL_MANAGER_TOKEN", "")
	t.Setenv("OPENSHELL_MANAGER_CONFIG", "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.ManagerURL = srv.URL
	return NewManagerRunner(cfg)
}

func TestInferenceAdmin_ListAndWorkspaceInjection(t *testing.T) {
	var gotWS string
	r := newInferenceRunner(t, Config{Workspace: "ws-x"}, func(w http.ResponseWriter, req *http.Request) {
		gotWS = req.URL.Query().Get("workspace")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers": []map[string]any{
				{"name": "prov-a", "type": "openai", "config": map[string]string{"base_url": "https://x/v1"}},
			},
		})
	})
	provs, err := r.ListInferenceProviders(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotWS != "ws-x" {
		t.Fatalf("workspace not injected from config: %q", gotWS)
	}
	if len(provs) != 1 || provs[0].Name != "prov-a" || provs[0].Config["base_url"] != "https://x/v1" {
		t.Fatalf("providers = %+v", provs)
	}
	// InferenceProvider 结构体本身无 credentials 字段——脱敏按类型强制（编译期纪律）
}

func TestInferenceAdmin_UpsertBodyAndAuth(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	var auth string
	r := newInferenceRunner(t, Config{Workspace: "ws-x"}, func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		_ = json.NewDecoder(req.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "prov-b", "created": true})
	})
	created, err := r.UpsertInferenceProvider(context.Background(), "prov-b", "anthropic",
		map[string]string{"api_key": "sk-x"}, map[string]string{"base_url": "https://y/v1"})
	if err != nil || !created {
		t.Fatalf("upsert: created=%v err=%v", created, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body["workspace"] != "ws-x" || body["name"] != "prov-b" || body["type"] != "anthropic" {
		t.Fatalf("upsert body = %v", body)
	}
	creds, _ := body["credentials"].(map[string]any)
	if creds["api_key"] != "sk-x" {
		t.Fatalf("credentials must pass through: %v", body)
	}
	if auth != "" {
		t.Fatalf("empty token must not send Authorization header, got %q", auth)
	}
}

func TestInferenceAdmin_DeleteAndTyped404(t *testing.T) {
	r := newInferenceRunner(t, Config{Workspace: "ws-x"}, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "provider 'nope' not found in workspace 'ws-x'"})
	})
	deleted, err := r.DeleteInferenceProvider(context.Background(), "nope")
	if deleted {
		t.Fatalf("deleted=true on 404")
	}
	var he *ManagerHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("want typed 404 error, got %v", err)
	}
	if he.Body == "" || he.Body == `{"error":"..."}` {
		t.Fatalf("error body must be the manager message, got %q", he.Body)
	}
}

func TestInferenceAdmin_SetRouteReceipt(t *testing.T) {
	var body map[string]any
	r := newInferenceRunner(t, Config{Workspace: "ws-x"}, func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"provider": "prov-a", "model": "m-9", "version": 7,
			"validation_performed": true,
			"validated_endpoints":  []map[string]string{{"url": "https://gw/v1", "protocol": "https"}},
		})
	})
	res, err := r.SetInferenceRoute(context.Background(), "prov-a", "m-9", false)
	if err != nil {
		t.Fatalf("set route: %v", err)
	}
	if body["no_verify"] != false || body["provider"] != "prov-a" || body["model"] != "m-9" {
		t.Fatalf("set route body = %v", body)
	}
	if !res.ValidationPerformed || len(res.ValidatedEndpoints) != 1 ||
		res.ValidatedEndpoints[0].URL != "https://gw/v1" || res.Version != 7 {
		t.Fatalf("receipt = %+v", res)
	}
}

func TestInferenceAdmin_UnreachableIsPlainError(t *testing.T) {
	r := newInferenceRunner(t, Config{Workspace: "ws-x"}, func(w http.ResponseWriter, req *http.Request) {})
	r.cfg.ManagerURL = "http://127.0.0.1:1" // 不可达端口
	_, err := r.GetInferenceRoute(context.Background())
	if err == nil {
		t.Fatalf("want error")
	}
	var he *ManagerHTTPError
	if errors.As(err, &he) {
		t.Fatalf("transport failure must not be typed HTTP error: %v", err)
	}
}
