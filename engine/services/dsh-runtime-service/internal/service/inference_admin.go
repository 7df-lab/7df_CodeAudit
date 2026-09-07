// DSHRuntimeService 推理管理面 6 RPC（ADR-217）：纯管道——经 openshell-manager
// 透传 OpenShell 网关，本服务不持有 provider 状态。workspace 从全局配置注入。
// 依据: codeaudit_common.proto DSHRuntimeService ListInferenceProviders…SetInferenceRoute
package service

import (
	"context"
	"errors"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inferenceRunner — 从全局配置装配管理面 runner（与 analyzeViaSandbox 同源装配，
// ADR-137 缺键 fail-fast；manager 不可达在调用时以 Unavailable fail-loud）。
func inferenceRunner() (*sandbox.ManagerRunner, error) {
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, err
	}
	return sandbox.NewManagerRunner(*cfg), nil
}

// managerErrToGRPC — manager 错误 → gRPC 状态码：HTTP 404→NotFound、400→InvalidArgument、
// 401/403→PermissionDenied、其余（含不可达）→Unavailable（网关映射 503，诚实降级口径）。
func managerErrToGRPC(err error) error {
	if err == nil {
		return nil
	}
	var he *sandbox.ManagerHTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 400:
			return status.Errorf(codes.InvalidArgument, "manager: %s", he.Body)
		case 404:
			return status.Errorf(codes.NotFound, "manager: %s", he.Body)
		case 401, 403:
			return status.Errorf(codes.PermissionDenied, "manager: %s", he.Body)
		default:
			return status.Errorf(codes.Unavailable, "manager HTTP %d: %s", he.Status, he.Body)
		}
	}
	return status.Errorf(codes.Unavailable, "%v", err)
}

func requireRequestID(md *pb.RequestMetadata) error {
	if md == nil || md.GetRequestId() == "" {
		return status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	return nil
}

// ListInferenceProviders — provider 概要清单（无凭据）。
func (s *DSHRuntimeServiceImpl) ListInferenceProviders(ctx context.Context, _ *pb.ListInferenceProvidersRequest) (*pb.ListInferenceProvidersResponse, error) {
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	provs, err := r.ListInferenceProviders(ctx)
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	out := &pb.ListInferenceProvidersResponse{}
	for _, p := range provs {
		out.Providers = append(out.Providers, &pb.InferenceProviderInfo{
			Name: p.Name, Type: p.Type, Config: p.Config,
		})
	}
	return out, nil
}

// GetInferenceProvider — 单个 provider（不存在 → NotFound）。
func (s *DSHRuntimeServiceImpl) GetInferenceProvider(ctx context.Context, req *pb.GetInferenceProviderRequest) (*pb.InferenceProviderInfo, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	p, err := r.GetInferenceProvider(ctx, req.GetName())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.InferenceProviderInfo{Name: p.Name, Type: p.Type, Config: p.Config}, nil
}

// UpsertInferenceProvider — 创建/更新（幂等键必填 R4；upsert 天然幂等，同键同体重放
// 结果一致，无需响应缓存）。created=true 走 Create，false 走 Update（manager 判定）。
func (s *DSHRuntimeServiceImpl) UpsertInferenceProvider(ctx context.Context, req *pb.UpsertInferenceProviderRequest) (*pb.UpsertInferenceProviderResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" || req.GetType() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and type are required")
	}
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	created, err := r.UpsertInferenceProvider(ctx, req.GetName(), req.GetType(),
		req.GetCredentials(), req.GetConfig())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.UpsertInferenceProviderResponse{Name: req.GetName(), Created: created}, nil
}

// DeleteInferenceProvider — 删除（幂等：不存在 → deleted=false）。
func (s *DSHRuntimeServiceImpl) DeleteInferenceProvider(ctx context.Context, req *pb.DeleteInferenceProviderRequest) (*pb.DeleteInferenceProviderResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	deleted, err := r.DeleteInferenceProvider(ctx, req.GetName())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.DeleteInferenceProviderResponse{Deleted: deleted}, nil
}

// GetInferenceRoute — 当前工作区推理路由（未设置时 provider/model 空串）。
func (s *DSHRuntimeServiceImpl) GetInferenceRoute(ctx context.Context, _ *pb.GetInferenceRouteRequest) (*pb.InferenceRouteInfo, error) {
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	rt, err := r.GetInferenceRoute(ctx)
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.InferenceRouteInfo{Provider: rt.Provider, Model: rt.Model, Version: rt.Version}, nil
}

// SetInferenceRoute — 切路由；no_verify=false 时网关连通性验证，回执带 validated_endpoints。
func (s *DSHRuntimeServiceImpl) SetInferenceRoute(ctx context.Context, req *pb.SetInferenceRouteRequest) (*pb.SetInferenceRouteResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetProvider() == "" || req.GetModel() == "" {
		return nil, status.Error(codes.InvalidArgument, "provider and model are required")
	}
	r, err := inferenceRunner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	res, err := r.SetInferenceRoute(ctx, req.GetProvider(), req.GetModel(), req.GetNoVerify())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	out := &pb.SetInferenceRouteResponse{
		Provider: res.Provider, Model: res.Model, Version: res.Version,
		ValidationPerformed: res.ValidationPerformed,
	}
	for _, e := range res.ValidatedEndpoints {
		out.ValidatedEndpoints = append(out.ValidatedEndpoints,
			&pb.ValidatedEndpoint{Url: e.URL, Protocol: e.Protocol})
	}
	return out, nil
}
