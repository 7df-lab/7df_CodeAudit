package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// HealthResponse represents the health check response
type HealthResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Deps    map[string]string `json:"deps,omitempty"` // 后端分项（探针裁决 ADR-250：503 时排障定位面）
}

// healthProbeTimeout — 单后端 gRPC health 探测超时（聚合探针并行探六后端，
// 总耗时≈单后端上限；compose healthcheck 10s 间隔下开销可忽略）。
const healthProbeTimeout = 800 * time.Millisecond

// HealthHandler handles health check requests
// 依据: 01 §4 (gateway-service health check)；P0 聚合真探（ADR-250，HA 设计
// platform-ha-2026-09-19 §4）：此前恒返 ok 不探后端——编排器（compose/k8s 探针）
// 无从感知后端死活，"摘掉任一后端 30s 内转红"是 HA 一切的前提。六后端 gRPC
// health 并行探测（空 service 名=整体 SERVING），任一非 SERVING/不可达 → 503
// +分项 body；全绿 → 200。
func (t *Transcoder) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error": "method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	deps := map[string]*grpc.ClientConn{
		"project":      t.projectConn,
		"task":         t.taskConn,
		"result":       t.resultConn,
		"storage":      t.storageConn,
		"sast-adapter": t.adapterConn,
		"runtime":      t.dshConn,
	}
	results := make(map[string]string, len(deps))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, conn := range deps {
		wg.Add(1)
		go func(name string, conn *grpc.ClientConn) {
			defer wg.Done()
			// 第四轮审计 P2：锁只护 results 写——原 mu.Lock 在 RPC 之前且 defer 到
			// 函数尾，六路探测被完全串行化（部分后端宕机时 /health 最长 6×800ms，
			// "并行探测"注释失真，compose healthcheck 逼近上限）。
			if conn == nil {
				mu.Lock()
				results[name] = "not-configured"
				mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
			defer cancel()
			resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				results[name] = "unreachable: " + err.Error()
			case resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING:
				results[name] = "not-serving: " + resp.GetStatus().String()
			default:
				results[name] = "ok"
			}
		}(name, conn)
	}
	wg.Wait()

	resp := HealthResponse{Service: "gateway-service", Deps: results}
	code := http.StatusOK
	resp.Status = "ok"
	for _, v := range results {
		if v != "ok" {
			code = http.StatusServiceUnavailable
			resp.Status = "degraded"
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}
