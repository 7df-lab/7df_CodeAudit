// 推理 provider/路由管理面（ADR-217）：经 openshell-manager 的 /api/v1/inference/*
// 透传 OpenShell 网关（权威存储在网关 gateway.db）。本文件只做传输：workspace 由
// 全局配置注入；credentials 只进不出（manager 侧对读响应按省略脱敏）。
// 与 Run/分析主链路的 call() 分离：管理面需要把 manager 的 HTTP 状态码保留为类型化
// 错误（供 gRPC 层映射 NotFound/InvalidArgument），不动主链路的错误契约。
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// InferenceProvider — manager providers 响应元素（凭据不在其中，manager 侧脱敏）。
type InferenceProvider struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

// InferenceRouteState — manager route 读响应。
type InferenceRouteState struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Version  uint64 `json:"version"`
}

// InferenceRouteSetResult — manager route 写响应（含网关连通性验证回执）。
type InferenceRouteSetResult struct {
	Provider             string             `json:"provider"`
	Model                string             `json:"model"`
	Version              uint64             `json:"version"`
	ValidationPerformed  bool               `json:"validation_performed"`
	ValidatedEndpoints   []ValidatedEndpointInfo `json:"validated_endpoints"`
}

// ValidatedEndpointInfo — 网关验证通过的推理端点。
type ValidatedEndpointInfo struct {
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

// ManagerHTTPError — manager 非 2xx 响应的类型化错误（状态码保留供 gRPC 映射）。
type ManagerHTTPError struct {
	Status int
	Body   string
}

func (e *ManagerHTTPError) Error() string {
	return fmt.Sprintf("openshell-manager -> HTTP %d: %s", e.Status, e.Body)
}

// callMgr — manager JSON 调用（inference 管理面变体）：2xx → 解析进 out；
// 非 2xx → *ManagerHTTPError；传输失败 → 包含上下文的普通错误（调用方按不可达降级）。
func (r *ManagerRunner) callMgr(ctx context.Context, method, path string, body any, out any) error {
	base, token := r.managerEndpoint()
	full := base + path
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return fmt.Errorf("openshell-manager unreachable (%s %s): %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("openshell-manager read body (%s %s): %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if s, ok := m["error"].(string); ok {
				msg = s
			}
		}
		return &ManagerHTTPError{Status: resp.StatusCode, Body: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("openshell-manager %s %s: non-JSON body: %w", method, path, err)
	}
	return nil
}

func (r *ManagerRunner) wsQuery() url.Values {
	q := url.Values{}
	ws := r.cfg.Workspace
	if ws == "" {
		ws = "default"
	}
	q.Set("workspace", ws)
	return q
}

// ListInferenceProviders — provider 概要清单（name/type/config，无凭据）。
func (r *ManagerRunner) ListInferenceProviders(ctx context.Context) ([]InferenceProvider, error) {
	var out struct {
		Providers []InferenceProvider `json:"providers"`
	}
	if err := r.callMgr(ctx, http.MethodGet,
		"/api/v1/inference/providers?"+r.wsQuery().Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

// GetInferenceProvider — 单个 provider（不存在 → *ManagerHTTPError 404）。
func (r *ManagerRunner) GetInferenceProvider(ctx context.Context, name string) (*InferenceProvider, error) {
	var out InferenceProvider
	q := r.wsQuery()
	if err := r.callMgr(ctx, http.MethodGet,
		"/api/v1/inference/providers/"+url.PathEscape(name)+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpsertInferenceProvider — 创建/更新（manager 判存在性，回执 created 标识）。
// credentials 只进不出：透传 manager → 网关加密存储，任何读路径不回流。
func (r *ManagerRunner) UpsertInferenceProvider(ctx context.Context, name, typ string, credentials, config map[string]string) (created bool, err error) {
	body := map[string]any{
		"workspace":   r.cfg.Workspace,
		"name":        name,
		"type":        typ,
		"credentials": credentials,
		"config":      config,
	}
	var out struct {
		Name    string `json:"name"`
		Created bool   `json:"created"`
	}
	if err := r.callMgr(ctx, http.MethodPut, "/api/v1/inference/providers", body, &out); err != nil {
		return false, err
	}
	return out.Created, nil
}

// DeleteInferenceProvider — 删除（幂等：不存在 → deleted=false）。
func (r *ManagerRunner) DeleteInferenceProvider(ctx context.Context, name string) (deleted bool, err error) {
	var out struct {
		Name    string `json:"name"`
		Deleted bool   `json:"deleted"`
	}
	q := r.wsQuery()
	if err := r.callMgr(ctx, http.MethodDelete,
		"/api/v1/inference/providers/"+url.PathEscape(name)+"?"+q.Encode(), nil, &out); err != nil {
		return false, err
	}
	return out.Deleted, nil
}

// GetInferenceRoute — 当前工作区推理路由（未设置时 provider/model 为空串）。
func (r *ManagerRunner) GetInferenceRoute(ctx context.Context) (*InferenceRouteState, error) {
	var out InferenceRouteState
	if err := r.callMgr(ctx, http.MethodGet,
		"/api/v1/inference/route?"+r.wsQuery().Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetInferenceRoute — 切路由（noVerify=false 时网关做连通性验证并回执端点）。
func (r *ManagerRunner) SetInferenceRoute(ctx context.Context, provider, model string, noVerify bool) (*InferenceRouteSetResult, error) {
	body := map[string]any{
		"workspace": r.cfg.Workspace,
		"provider":  provider,
		"model":     model,
		"no_verify": noVerify,
	}
	var out InferenceRouteSetResult
	if err := r.callMgr(ctx, http.MethodPut, "/api/v1/inference/route", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
