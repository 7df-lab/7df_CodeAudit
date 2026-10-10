package service

// 编排 CRUD 批单测：Pack 生命周期 RPC（append-only 修订语义）。
// 内存档（无 PG）——deprecated 走 Registry 内存集。

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/codeaudit/proto-gen"
)

const auditGraphYAML = `apiVersion: codeaudit.pack/v0
kind: WorkloadPack
name: orch-demo
description: 单测编排（图模式）
spec:
  image: dsh-pentest-sse:latest
  dialect: dsh-sse
  network:
    profile: restricted
  execution: graph
  seeds:
    brief_renderer: baton-brief
    render_target: task-card-prefix
    template_ref: assignmentTemplate@frozen
  results:
    schema_ref: findings/submit_findings@v1
    parser: codeaudit-submit
  timeouts:
    per_turn: default
    task_total: audit-default
  graph:
    seed_intents:
      - "危险模式审计面"
    constraints:
      max_nodes: 8
      allowed_types: [planner, executor, verifier]
      max_planner_replans: 2
      max_attempts_per_node: 2
    timeouts:
      planner: 120
      executor: 600
      verifier: 240
`

func orchSvc(t *testing.T) (*TaskServiceImpl, context.Context) {
	t.Helper()
	s := newSvc(t)
	s.builtinPacks = map[string]bool{"builtin-std": true}
	md := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-user-id", "admin-1"))
	return s, md
}

func TestOrchestration_CRUDFlow(t *testing.T) {
	s, ctx := orchSvc(t)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-user-id", "admin-1")

	// Create：revision=1，readonly=false，created_by 注入
	info, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-demo", SpecYaml: auditGraphYAML})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.GetLatestRevision() != 1 || info.GetReadonly() || info.GetDeprecated() {
		t.Fatalf("create Info 失真: %+v", info)
	}
	if info.GetExecution() != "graph" || info.GetDescription() != "单测编排（图模式）" {
		t.Fatalf("execution/description 失真: %+v", info)
	}

	// 同 name 重复 create → AlreadyExists（提示走更新接口；append 语义下不做静默幂等）
	_, err = s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-demo", SpecYaml: auditGraphYAML})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("重复 create 应 AlreadyExists: %v", err)
	}

	// Update：append 修订 2（真实内容变更：max_nodes 12→10；描述同步刷新）
	tuned := strings.Replace(auditGraphYAML, "max_nodes: 8", "max_nodes: 10", 1)
	info3, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "orch-demo", SpecYaml: tuned})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if info3.GetLatestRevision() != 2 {
		t.Fatalf("update 应 append revision=2: %d", info3.GetLatestRevision())
	}
	if !strings.Contains(info3.GetSpecYaml(), "max_nodes: 10") {
		t.Fatal("spec_yaml 未反映新修订")
	}

	// 同 spec 重复 Update → 复用不追加（revision 停在 2；哈希命中最新修订）
	info4, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "orch-demo", SpecYaml: tuned})
	if err != nil {
		t.Fatalf("reused update: %v", err)
	}
	if info4.GetLatestRevision() != 2 {
		t.Fatalf("同 spec 重复提交不应追加修订: %d", info4.GetLatestRevision())
	}
	if info4.GetSpecHash() != info3.GetSpecHash() {
		t.Fatal("复用修订哈希漂移")
	}

	// 新修订=复活且持久化（对抗式审查 P2-3）：hook 收到 (name,false)——不落则 PG
	// 回放把停用态复活回来，重启后静默翻转
	var revived []string
	s.packs.SetDeprecatedHook(func(name string, dep bool) error {
		if !dep {
			revived = append(revived, name)
		}
		return nil
	})
	s.packs.SetDeprecated("orch-demo", true)
	revived = nil // 排除停用动作自身的 hook 噪音
	infoR, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "orch-demo", SpecYaml: strings.Replace(tuned, "max_nodes: 10", "max_nodes: 11", 1)})
	if err != nil {
		t.Fatalf("revive update: %v", err)
	}
	if infoR.GetDeprecated() {
		t.Fatal("新修订应复活（内存面）")
	}
	if len(revived) != 1 || revived[0] != "orch-demo" {
		t.Fatalf("复活未持久化（对抗式审查 P2-3）: %v", revived)
	}
	s.packs.SetDeprecatedHook(func(string, bool) error { return nil })

	// 同 spec 仅 description 变 → 元数据刷新不产生新修订（revision 不变、描述可见）
	// spec 基面=复活修订（max_nodes: 11）——仅 description 差异才走复用刷新分支
	headBefore := s.packs.LatestAll()["orch-demo"] // immutable 锚：刷新前共享指针
	retuned := strings.Replace(strings.Replace(tuned, "max_nodes: 10", "max_nodes: 11", 1), "单测编排（图模式）", "刷新后的描述", 1)
	info5, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "orch-demo", SpecYaml: retuned})
	if err != nil {
		t.Fatalf("metadata refresh update: %v", err)
	}
	if info5.GetLatestRevision() != 3 {
		t.Fatalf("元数据刷新不应追加修订: %d", info5.GetLatestRevision())
	}
	if info5.GetDescription() != "刷新后的描述" {
		t.Fatalf("元数据刷新不可见: %q", info5.GetDescription())
	}
	if headBefore.Description == "刷新后的描述" {
		t.Fatal("刷新前持有的共享指针被原地改写（Revision immutable 破坏=读侧竞态根）")
	}

	// List：include_history → 2 条史；默认过滤停用
	list, err := s.ListOrchestrations(ctx, &pb.ListOrchestrationsRequest{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	var found *pb.OrchestrationInfo
	for _, o := range list.GetOrchestrations() {
		if o.GetName() == "orch-demo" {
			found = o
		}
	}
	if found == nil || len(found.GetHistory()) != 3 {
		t.Fatalf("修订史缺失: %+v", found)
	}
	// created_by 注入（x-user-id metadata → 修订史；网关 callerCtx 已接）
	if got := found.GetHistory()[0].GetCreatedBy(); got != "admin-1" {
		t.Fatalf("history[0].created_by = %q, want admin-1", got)
	}
	if got := found.GetHistory()[1].GetCreatedBy(); got != "admin-1" {
		t.Fatalf("history[1].created_by = %q, want admin-1", got)
	}
	if got := found.GetHistory()[2].GetCreatedBy(); got != "admin-1" {
		t.Fatalf("history[2].created_by = %q, want admin-1", got)
	}
}

func TestOrchestration_DeprecateAndRestore(t *testing.T) {
	s, ctx := orchSvc(t)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-user-id", "admin-1")
	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-demo", SpecYaml: auditGraphYAML}); err != nil {
		t.Fatal(err)
	}
	info, err := s.DeprecateOrchestration(ctx, &pb.DeprecateOrchestrationRequest{Name: "orch-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !info.GetDeprecated() {
		t.Fatal("停用标记未生效")
	}
	// List 默认过滤停用；include_deprecated=true 含之
	list, err := s.ListOrchestrations(ctx, &pb.ListOrchestrationsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list.GetOrchestrations() {
		if o.GetName() == "orch-demo" {
			t.Fatal("默认列表不应含停用编排")
		}
	}
	list2, err := s.ListOrchestrations(ctx, &pb.ListOrchestrationsRequest{IncludeDeprecated: true})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, o := range list2.GetOrchestrations() {
		if o.GetName() == "orch-demo" {
			seen = true
			if !o.GetDeprecated() {
				t.Fatal("include_deprecated 投影缺停用标记")
			}
		}
	}
	if !seen {
		t.Fatal("include_deprecated=true 应含停用编排")
	}
	// 恢复
	info2, err := s.DeprecateOrchestration(ctx, &pb.DeprecateOrchestrationRequest{Name: "orch-demo", UndePrecate: true})
	if err != nil {
		t.Fatal(err)
	}
	if info2.GetDeprecated() {
		t.Fatal("恢复失败")
	}
}

// 已停用编排拒用于新建任务（CreateScanTask 的 pack 键消费面——并发会话 2026-09-21
// WIP 接手收尾批补测；停用面=ListOrchestrations 默认过滤+建任务拒用两面闭环）。
func TestCreateScanTask_RejectsDeprecatedPack(t *testing.T) {
	s, ctx := orchSvc(t)
	depYAML := strings.ReplaceAll(auditGraphYAML, "orch-demo", "orch-dep") // name 与请求对齐（比对纪律）
	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-dep", SpecYaml: depYAML}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeprecateOrchestration(ctx, &pb.DeprecateOrchestrationRequest{Name: "orch-dep"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "dep-1"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"pack": "orch-dep"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("deprecated pack create = %v, want InvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "已停用") {
		t.Fatalf("error 文案缺已停用语义: %v", err)
	}
	// 恢复后可建（unde_precate 语义闭环）
	if _, err := s.DeprecateOrchestration(ctx, &pb.DeprecateOrchestrationRequest{Name: "orch-dep", UndePrecate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "dep-2"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"pack": "orch-dep"},
	}); err != nil {
		t.Fatalf("restored pack create: %v", err)
	}
}

func TestOrchestration_BuiltinGuards(t *testing.T) {
	s, ctx := orchSvc(t)
	s.builtinPacks["builtin-std"] = true
	ctx = metadata.AppendToOutgoingContext(ctx, "x-user-id", "admin-1")

	// 真实装载形态：内置 pack 同时注册在 Registry（task_service 装载路径同款），
	// builtinPacks 是只读标记而非唯一事实——Update 守卫必须先于存在性检查生效。
	builtinYAML := strings.ReplaceAll(auditGraphYAML, "orch-demo", "builtin-std")
	bp, err := orchestrationPackOf("builtin-std", builtinYAML)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.packs.Register(bp, "platform"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "builtin-std", SpecYaml: builtinYAML}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("内置同名新建应 AlreadyExists: %v", err)
	}
	if _, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "builtin-std", SpecYaml: builtinYAML}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("内置在册时 Update 应 FailedPrecondition（修复前：LatestAll 存在→Register 成功=内置被改写）: %v", err)
	}
	// 守卫生效后内置修订史未被污染（仍恰 1 条 platform 修订）
	if h := s.packs.History("builtin-std"); len(h) != 1 || h[0].CreatedBy != "platform" {
		t.Fatalf("内置修订史被污染: %+v", h)
	}
	if _, err := s.DeprecateOrchestration(ctx, &pb.DeprecateOrchestrationRequest{Name: "builtin-std"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("内置停用应 FailedPrecondition: %v", err)
	}
}

func TestOrchestration_ValidationErrors(t *testing.T) {
	s, ctx := orchSvc(t)
	// name 不一致用完整合法 spec（修复前该用例靠 spec 必填失败假绿——name 比对不存在）
	diverted := strings.Replace(auditGraphYAML, "name: orch-demo", "name: other", 1)
	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-demo", SpecYaml: diverted}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("yaml.name 与请求 name 不一致应 InvalidArgument（含比对语义）: %v", err)
	}
	// 未知字段拒载（KnownFields——与内置装载 pack.Parse 同纪律）
	bogus := auditGraphYAML + "  bogus_field: 1\n"
	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "orch-demo", SpecYaml: bogus}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "bogus_field") {
		t.Fatalf("未知字段应 InvalidArgument 并指认字段名: %v", err)
	}
	if _, err := s.CreateOrchestration(ctx, &pb.CreateOrchestrationRequest{Name: "y", SpecYaml: "apiVersion: nope\nkind: nope\nname: y\n"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("非法 yaml 应 InvalidArgument: %v", err)
	}
	if _, err := s.UpdateOrchestration(ctx, &pb.UpdateOrchestrationRequest{Name: "ghost", SpecYaml: strings.ReplaceAll(auditGraphYAML, "orch-demo", "ghost")}); status.Code(err) != codes.NotFound {
		t.Fatalf("更新不存在编排应 NotFound: %v", err)
	}
}
