package service

// orchestration_admin.go — 编排（负载 pack）生命周期 RPC（编排 CRUD 批 2026-09-21；
// 人类裁决立项）。设计=graph-reseed 后续+platform-slice2-pack-schema §3 修订语义：
// Update=append 新修订（任务钉修订不破坏，审计可重建）；Deprecate=停用标记（历史
// 引用永存）；内置编排（configs/packs 装载）只读。spec 以 YAML 全量传输（服务端
// Validate 校验，错误带字段路径回显前端）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/metadata"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/pack"
)

const (
	orchestrationAPIVersion = "codeaudit.pack/v0"
	orchestrationKind       = "WorkloadPack"
	orchestrationMaxYAML    = 64 << 10 // 64KB：单 pack YAML 上限（防滥用，防御面）
)

// orchestrationPackOf — spec_yaml → Pack。严格解析（KnownFields=未知字段拒载，
// 与内置装载 pack.Parse 同纪律——防笔误字段被静默丢弃）；yaml 内显式 name 与请求
// name 不一致即拒（name 权威=REST 路径段；比对先于覆盖，校验作用于最终生效值）。
func orchestrationPackOf(name, specYAML string) (*pack.Pack, error) {
	var p pack.Pack
	dec := yaml.NewDecoder(strings.NewReader(specYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("spec_yaml 解析失败: %v", err)
	}
	if p.Name != "" && p.Name != name {
		return nil, fmt.Errorf("spec_yaml 内 name %q 与请求 name %q 不一致", p.Name, name)
	}
	p.Name = name
	if err := p.Validate(nil); err != nil {
		return nil, err
	}
	return &p, nil
}

// orchestrationInfoOf — Registry 修订 → API 投影（spec_yaml=规范化全量 YAML）。
func (s *TaskServiceImpl) orchestrationInfoOf(name string, rev *pack.Revision, includeHistory bool) (*pb.OrchestrationInfo, error) {
	var sp pack.Spec
	if err := json.Unmarshal([]byte(rev.SpecJSON), &sp); err != nil {
		return nil, fmt.Errorf("spec_json 解析失败: %v", err)
	}
	full := pack.Pack{
		APIVersion:  orchestrationAPIVersion,
		Kind:        orchestrationKind,
		Name:        name,
		Description: rev.Description,
		Spec:        sp,
	}
	yb, err := yaml.Marshal(full)
	if err != nil {
		return nil, fmt.Errorf("spec_yaml 序列化: %v", err)
	}
	info := &pb.OrchestrationInfo{
		Name:           name,
		LatestRevision: int32(rev.Revision),
		SpecYaml:       string(yb),
		SpecHash:       rev.SpecHash,
		Execution:      sp.Execution,
		Description:    rev.Description,
		Deprecated:     s.packs.IsDeprecated(name),
		Readonly:       s.builtinPacks[name],
		UpdatedAt:      rev.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if includeHistory {
		for _, h := range s.packs.History(name) {
			info.History = append(info.History, &pb.OrchestrationRevision{
				Revision: int32(h.Revision), SpecHash: h.SpecHash,
				CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}
	return info, nil
}

// createdByFromCtx — 创建者身份（网关注入 JWT sub；与任务 created_by 同源）。
func createdByFromCtx(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-user-id"); len(v) > 0 {
		return v[0]
	}
	return ""
}

// ListOrchestrations — 编排目录（登录面；默认过滤停用）。
func (s *TaskServiceImpl) ListOrchestrations(ctx context.Context, req *pb.ListOrchestrationsRequest) (*pb.ListOrchestrationsResponse, error) {
	latest := s.packs.LatestAll()
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	sort.Strings(names)
	resp := &pb.ListOrchestrationsResponse{}
	for _, name := range names {
		rev := latest[name]
		dep := s.packs.IsDeprecated(name)
		if dep && !req.GetIncludeDeprecated() {
			continue
		}
		info, err := s.orchestrationInfoOf(name, rev, req.GetIncludeHistory())
		if err != nil {
			return nil, err
		}
		info.Deprecated = dep
		resp.Orchestrations = append(resp.Orchestrations, info)
	}
	return resp, nil
}

// CreateOrchestration — 新建编排（create-only：同 name 即 AlreadyExists——含
// 字节级同 spec，不做静默幂等，客户端重试走 List 确认；内置名保留拒建）。
func (s *TaskServiceImpl) CreateOrchestration(ctx context.Context, req *pb.CreateOrchestrationRequest) (*pb.OrchestrationInfo, error) {
	if req.GetName() == "" || req.GetSpecYaml() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "name 与 spec_yaml 必填")
	}
	if len(req.GetSpecYaml()) > orchestrationMaxYAML {
		return nil, status.Errorf(codes.InvalidArgument, "spec_yaml 超限（>%d 字节）", orchestrationMaxYAML)
	}
	p, err := orchestrationPackOf(req.GetName(), req.GetSpecYaml())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if s.builtinPacks[req.GetName()] {
		return nil, status.Errorf(codes.AlreadyExists, "编排 %q 为内置只读目录——不可同名新建", req.GetName())
	}
	rev, _, err := s.packs.RegisterIfAbsent(p, createdByFromCtx(ctx))
	if err != nil {
		if errors.Is(err, pack.ErrPackExists) {
			return nil, status.Errorf(codes.AlreadyExists, "编排 %q 已存在——变更走更新接口（append 新修订）", req.GetName())
		}
		return nil, status.Errorf(codes.Internal, "pack 注册: %v", err)
	}
	return s.orchestrationInfoOf(req.GetName(), rev, true)
}

// UpdateOrchestration — append 新修订（内置只读拒改；不存在=NotFound）。
func (s *TaskServiceImpl) UpdateOrchestration(ctx context.Context, req *pb.UpdateOrchestrationRequest) (*pb.OrchestrationInfo, error) {
	if req.GetName() == "" || req.GetSpecYaml() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "name 与 spec_yaml 必填")
	}
	if len(req.GetSpecYaml()) > orchestrationMaxYAML {
		return nil, status.Errorf(codes.InvalidArgument, "spec_yaml 超限（>%d 字节）", orchestrationMaxYAML)
	}
	p, err := orchestrationPackOf(req.GetName(), req.GetSpecYaml())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if s.builtinPacks[req.GetName()] {
		return nil, status.Errorf(codes.FailedPrecondition, "编排 %q 为内置只读目录——不可修改（内置守卫先于存在性：内置同时注册在 Registry）", req.GetName())
	}
	if _, exists := s.packs.LatestAll()[req.GetName()]; !exists {
		return nil, status.Errorf(codes.NotFound, "编排 %q 不存在（新建走创建接口）", req.GetName())
	}
	rev, _, err := s.packs.Register(p, createdByFromCtx(ctx))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pack 注册: %v", err)
	}
	return s.orchestrationInfoOf(req.GetName(), rev, true)
}

// DeprecateOrchestration — 停用/恢复（内置只读拒动）。停用后不进建任务可选与
// 默认列表；已钉修订的历史任务不受影响（append-only 语义）。
func (s *TaskServiceImpl) DeprecateOrchestration(ctx context.Context, req *pb.DeprecateOrchestrationRequest) (*pb.OrchestrationInfo, error) {
	if req.GetName() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "name 必填")
	}
	rev, exists := s.packs.LatestAll()[req.GetName()]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "编排 %q 不存在", req.GetName())
	}
	if s.builtinPacks[req.GetName()] {
		return nil, status.Errorf(codes.FailedPrecondition, "编排 %q 为内置只读目录——不可停用", req.GetName())
	}
	s.packs.SetDeprecated(req.GetName(), !req.GetUndePrecate())
	return s.orchestrationInfoOf(req.GetName(), rev, true)
}
