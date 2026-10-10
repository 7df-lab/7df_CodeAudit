package handler

// P0 聚合探针测试（ADR-250，HA 设计 §4）：六后端并行探测——全 SERVING→200；
// 任一后端不可达/非 SERVING→503+分项定位（"摘后端 30s 转红"验收门的单测面，
// compose 实测归 ha-verify 栈）。

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// startBackend — 内存 gRPC health 后端（serving=false=NOT_SERVING）。
func startBackend(t *testing.T, serving bool) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	if !serving {
		hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.DialContext(context.Background(), lis.Addr().String(),
		grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newHealthTranscoder(conns ...*grpc.ClientConn) *Transcoder {
	t := &Transcoder{}
	for i, c := range conns {
		switch i {
		case 0:
			t.projectConn = c
		case 1:
			t.taskConn = c
		case 2:
			t.resultConn = c
		case 3:
			t.storageConn = c
		case 4:
			t.adapterConn = c
		case 5:
			t.dshConn = c
		}
	}
	return t
}

func TestHealthAggregate_AllServing(t *testing.T) {
	tr := newHealthTranscoder(startBackend(t, true), startBackend(t, true), startBackend(t, true),
		startBackend(t, true), startBackend(t, true), startBackend(t, true))
	w := httptest.NewRecorder()
	tr.HealthHandler(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("all-serving must 200 ok, got %d %s", w.Code, w.Body.String())
	}
}

func TestHealthAggregate_OneDown(t *testing.T) {
	tr := newHealthTranscoder(startBackend(t, true), startBackend(t, false), startBackend(t, true),
		startBackend(t, true), startBackend(t, true), startBackend(t, true))
	w := httptest.NewRecorder()
	tr.HealthHandler(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "degraded") {
		t.Fatalf("one not-serving must 503 degraded, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"task":"not-serving`) {
		t.Fatalf("503 body must carry per-dep detail, got %s", w.Body.String())
	}
}

func TestHealthAggregate_Unreachable(t *testing.T) {
	dead := startBackend(t, true)
	addr := dead.Target()
	dead.Close() // 立即关闭→不可达
	_ = addr
	// 直接构造一个指向无监听地址的 conn
	conn, err := grpc.DialContext(context.Background(), "127.0.0.1:1",
		grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	tr := newHealthTranscoder(conn, startBackend(t, true), startBackend(t, true),
		startBackend(t, true), startBackend(t, true), startBackend(t, true))
	w := httptest.NewRecorder()
	tr.HealthHandler(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "unreachable") {
		t.Fatalf("unreachable backend must 503 with unreachable detail, got %d %s", w.Code, w.Body.String())
	}
}

func TestHealthAggregate_NotConfigured(t *testing.T) {
	tr := &Transcoder{} // 全 nil conn
	w := httptest.NewRecorder()
	tr.HealthHandler(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not-configured") {
		t.Fatalf("nil conn must 503 not-configured, got %d %s", w.Code, w.Body.String())
	}
}
