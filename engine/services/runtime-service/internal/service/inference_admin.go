// RuntimeService 推理管理面 6 RPC（ADR-217）：纯管道——经 openshell-manager
// 透传 OpenShell 网关，本服务不持有 provider 状态。workspace 从全局配置注入。
// 依据: codeaudit_common.proto RuntimeService ListInferenceProviders…SetInferenceRoute
package service

import (
	"context"
	"errors"
	"regexp"
	"strings"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inferenceRunner — 从全局配置装配管理面 runner（与 analyzeViaSandbox 同源装配，
// ADR-137 缺键 fail-fast；manager 不可达在调用时以 Unavailable fail-loud）。
// workspace 非空时覆盖全局缺省（ADR-247 切片3后半：per-team 命名空间配置入口）。
func inferenceRunner(workspace string) (*sandbox.ManagerRunner, error) {
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, err
	}
	if workspace != "" {
		cfg.Workspace = workspace
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
		case 409:
			// 切片3b 扩面：workspace 重名/删区资源守卫
			return status.Errorf(codes.FailedPrecondition, "manager: %s", he.Body)
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
func (s *RuntimeServiceImpl) ListInferenceProviders(ctx context.Context, req *pb.ListInferenceProvidersRequest) (*pb.ListInferenceProvidersResponse, error) {
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247: 空=全局缺省
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

// ListWorkspaces — workspace 清单（切片3b team 上车：web Provider 页选择器数据源；
// 只读透传，建/删区走 manager 运营面）。runner 仅取 manager 端点装配，清单本身
// 非 workspace 作用域。
func (s *RuntimeServiceImpl) ListWorkspaces(ctx context.Context, req *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	if req.GetLimit() < 0 || req.GetOffset() < 0 {
		return nil, status.Error(codes.InvalidArgument, "limit/offset must be >= 0")
	}
	r, err := inferenceRunner("")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	wss, err := r.ListWorkspaces(ctx, int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	out := &pb.ListWorkspacesResponse{}
	for _, w := range wss {
		out.Workspaces = append(out.Workspaces, &pb.WorkspaceInfo{
			Name: w.Name, Phase: w.Phase, Labels: w.Labels,
		})
	}
	return out, nil
}

// CreateWorkspace — 建区（web 管理页通道；R4 幂等键必填）。
func (s *RuntimeServiceImpl) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.WorkspaceInfo, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner("")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	w, err := r.CreateWorkspace(ctx, req.GetName(), req.GetLabels())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.WorkspaceInfo{Name: w.Name, Phase: w.Phase, Labels: w.Labels}, nil
}

// DeleteWorkspace — 删区（资源守卫 409→FailedPrecondition details 原文）。
func (s *RuntimeServiceImpl) DeleteWorkspace(ctx context.Context, req *pb.DeleteWorkspaceRequest) (*pb.DeleteWorkspaceResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner("")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	deleted, err := r.DeleteWorkspace(ctx, req.GetName())
	if err != nil {
		return nil, managerErrToGRPC(err)
	}
	return &pb.DeleteWorkspaceResponse{Name: req.GetName(), Deleted: deleted}, nil
}

// GetInferenceProvider — 单个 provider（不存在 → NotFound）。
func (s *RuntimeServiceImpl) GetInferenceProvider(ctx context.Context, req *pb.GetInferenceProviderRequest) (*pb.InferenceProviderInfo, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247
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
func (s *RuntimeServiceImpl) UpsertInferenceProvider(ctx context.Context, req *pb.UpsertInferenceProviderRequest) (*pb.UpsertInferenceProviderResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" || req.GetType() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and type are required")
	}
	// R55: 网关（上游闭源件）按类型
	// 约定键解析端点——openai 系认 config.OPENAI_BASE_URL / credentials.OPENAI_API_KEY，
	// anthropic 认 ANTHROPIC_BASE_URL / API_KEY（网关 b448ef29 起 BASE_URL 已死，静默
	// 回落官方端点）。别名/死键会被静默存储但不被识别，切路由连通性验证时才失败（用户
	// 无从归因）。入口拦截已知别名与死键，报错指路该类型的约定键名；其余自定义键不受影响。
	if err := rejectInferenceAliasKeys(req.GetType(), req.GetConfig(), req.GetCredentials()); err != nil {
		return nil, err
	}
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247
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
func (s *RuntimeServiceImpl) DeleteInferenceProvider(ctx context.Context, req *pb.DeleteInferenceProviderRequest) (*pb.DeleteInferenceProviderResponse, error) {
	if err := requireRequestID(req.GetMetadata()); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247
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
func (s *RuntimeServiceImpl) GetInferenceRoute(ctx context.Context, req *pb.GetInferenceRouteRequest) (*pb.InferenceRouteInfo, error) {
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247
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
func (s *RuntimeServiceImpl) SetInferenceRoute(ctx context.Context, req *pb.SetInferenceRouteRequest) (*pb.SetInferenceRouteResponse, error) {
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
	r, err := inferenceRunner(req.GetWorkspace()) // ADR-247
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

// 2026-09-17 口径反转（R55 纠偏，网关镜像 b448ef29 实测钉死，manager api-external
// 3.16/3.19 同批契约化）：类型仅六种 openai/anthropic/nvidia/deepinfra/google-vertex-ai/
// aws-bedrock（deepseek/zhipu 是厂商不是类型，走 openai/anthropic 兼容协议）；
// anthropic 型端点键 = ANTHROPIC_BASE_URL——旧约定键 BASE_URL 被新镜像无视（静默回落
// 官方端点、区域 403），从放行表反转进拦截表；DEEPSEEK_* 随 deepseek 型除名一并撤出
// 放行表（小写形态在拦截表指路 OPENAI_*）。anthropic credentials 键仍是 API_KEY。
var inferenceCanonicalKeys = map[string]bool{
	"OPENAI_BASE_URL": true, "OPENAI_API_KEY": true,
	"ANTHROPIC_BASE_URL": true,
	"API_KEY":            true,
}

// inferenceAliasKeys — 已知别名/死键（大小写不敏感命中）→ [openai 系指路键, anthropic 指路键]。
// R55：小写别名键静默存储不被识别；R55 补遗：约定键全小写形态一并拦。
// 2026-09-17 反转：BASE_URL（旧 anthropic 约定键，现为死键）经 base_url 条目拦截并指路
// ANTHROPIC_BASE_URL；新增 ANTHROPIC_BASE_URL 的全小写形态拦截（对齐补遗纪律）。
var inferenceAliasKeys = map[string][2]string{
	"base_url":           {"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"},
	"baseurl":            {"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"},
	"api_key":            {"OPENAI_API_KEY", "API_KEY"},
	"apikey":             {"OPENAI_API_KEY", "API_KEY"},
	"openai_base_url":    {"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"},
	"openai_api_key":     {"OPENAI_API_KEY", "API_KEY"},
	"anthropic_base_url": {"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"},
	"anthropic_api_key":  {"OPENAI_API_KEY", "API_KEY"},
	"deepseek_base_url":  {"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"},
	"deepseek_api_key":   {"OPENAI_API_KEY", "API_KEY"},
}

// rejectInferenceAliasKeys — 已知别名/死键（大小写不敏感命中、且非精确约定键）→ InvalidArgument。
func rejectInferenceAliasKeys(typ string, config, credentials map[string]string) error {
	anthropic := strings.EqualFold(typ, "anthropic")
	inspect := func(m map[string]string) error {
		for k := range m {
			if inferenceCanonicalKeys[k] {
				continue // 精确约定键放行
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
				"键 %q 不被网关识别：网关按类型认约定键（type=%s 应为 %q），该键会被静默忽略导致路由验证失败（R55）；请改用约定键", k, typ, want)
		}
		return nil
	}
	if err := inspect(config); err != nil {
		return err
	}
	return inspect(credentials)
}
