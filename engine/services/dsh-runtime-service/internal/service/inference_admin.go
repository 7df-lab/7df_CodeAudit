// DSHRuntimeService 推理管理面 6 RPC（ADR-217）：纯管道——经 openshell-manager
// 透传 OpenShell 网关，本服务不持有 provider 状态。workspace 从全局配置注入。
// 依据: codeaudit_common.proto DSHRuntimeService ListInferenceProviders…SetInferenceRoute
package service

import (
	"context"
	"regexp"
	"errors"
	"strings"

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
	// R55（2026-09-11 报障修复）: 网关（上游闭源件）按约定大写键解析端点——openai 系认
	// config.OPENAI_BASE_URL / credentials.OPENAI_API_KEY，anthropic 认 BASE_URL/API_KEY。
	// 小写别名键会被静默存储但不被识别，切路由连通性验证时才失败（用户无从归因）。
	// 入口拦截常见别名键，报错指路约定键名；其余自定义键不受影响。
	if err := rejectInferenceAliasKeys(req.GetType(), req.GetConfig(), req.GetCredentials()); err != nil {
		return nil, err
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
	// R80: model 名白名单——route.Model 会被烘进沙箱 launchScript（bash -c，
	// session.go launch），空格即分词断裂（launch 必败难归因）、shell 元字符即
	// 沙箱内命令注入。写入口为主防线，读路径 shQuoteLite 兜底。
	if !modelSafePattern.MatchString(req.GetModel()) {
		return nil, status.Error(codes.InvalidArgument, "model name must match [A-Za-z0-9._:/-]+")
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


// inferenceCanonicalKeys — 网关约定的端点/凭据键（精确大小写），直接放行。
// modelSafePattern — R80: 路由 model 名字符白名单（写入口主防线）。
var modelSafePattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

var inferenceCanonicalKeys = map[string]bool{
	"OPENAI_BASE_URL": true, "OPENAI_API_KEY": true,
	"BASE_URL": true, "API_KEY": true,
	"DEEPSEEK_BASE_URL": true, "DEEPSEEK_API_KEY": true,
}

// inferenceAliasKeys — 小写别名 → [openai 系约定键, anthropic 约定键]。
// R55 补遗约定键的全小写形态（openai_base_url 等）比裸别名更
// 常见的直觉输入，同样静默存储不被网关识别——一并拦截。
var inferenceAliasKeys = map[string][2]string{
	"base_url":         {"OPENAI_BASE_URL", "BASE_URL"},
	"baseurl":          {"OPENAI_BASE_URL", "BASE_URL"},
	"api_key":          {"OPENAI_API_KEY", "API_KEY"},
	"apikey":           {"OPENAI_API_KEY", "API_KEY"},
	"openai_base_url":  {"OPENAI_BASE_URL", "BASE_URL"},
	"openai_api_key":   {"OPENAI_API_KEY", "API_KEY"},
	"deepseek_base_url": {"DEEPSEEK_BASE_URL", "BASE_URL"},
	"deepseek_api_key":  {"DEEPSEEK_API_KEY", "API_KEY"},
}

// rejectInferenceAliasKeys — 已知别名键（大小写不敏感命中、且非精确约定键）→ InvalidArgument。
func rejectInferenceAliasKeys(typ string, config, credentials map[string]string) error {
	anthropic := strings.EqualFold(typ, "anthropic")
	inspect := func(m map[string]string) error {
		for k := range m {
			if inferenceCanonicalKeys[k] {
				continue // 精确约定键（含 anthropic 的 BASE_URL/API_KEY）放行
			}
			pair, alias := inferenceAliasKeys[strings.ToLower(k)]
			if !alias {
				continue
			}
			want := pair[0]
			if anthropic {
				want = pair[1]
			}
			return status.Errorf(codes.InvalidArgument,
				"键 %q 是网关约定键的别名：网关只认约定大写键 %q（type=%s），小写键会被静默忽略导致路由验证失败（R55）", k, want, typ)
		}
		return nil
	}
	if err := inspect(config); err != nil {
		return err
	}
	return inspect(credentials)
}
