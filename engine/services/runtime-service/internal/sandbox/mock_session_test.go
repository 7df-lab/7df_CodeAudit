package sandbox

// mock-stdio 方言全路径测试（R6 v0，切片 1）：descriptor 条目纯数据驱动 Run 编排全链，
// 4 处消费点零改动——R6 原验收"加一个 descriptor 条目即可拉起新 agent"的 mock 闭环。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §4.1（mock 先行）/§5（DoD：
// mock 条目验收闭环 + capability unsupported 路径有测试）。

import (
	"context"
	"strings"
	"testing"
)

// mockDescriptor — 测试用 mock 条目（镜像 agent_providers.yaml 条目形状）。
func mockDescriptor(id string, caps Capabilities) ProviderDescriptor {
	d := DefaultDescriptor()
	d.ID = id
	d.Dialect = "mock-stdio"
	d.Launch = LaunchSpec{PermissionMode: "danger-full-access"}
	d.Session = SessionSpec{Transport: "mock-inproc", IdleConvergence: "turn-based", Resume: "none"}
	parser := "codeaudit-submit"
	if !caps.StructuredSubmit {
		parser = "text-final" // text-final ⇔ structured_submit=false 一致性（设计 §3）
	}
	d.Results = ResultsSpec{Parser: parser}
	d.Seeds = SeedsSpec{BriefRenderer: "baton-brief", RenderTarget: "task-card-prefix"}
	d.Capabilities = caps
	d.Healthcheck = HealthSpec{Mode: "none"}
	return d
}

func mockRunner(t *testing.T, d ProviderDescriptor, events *[]string) *ManagerRunner {
	t.Helper()
	t.Setenv("CODEAUDIT_MOCK_DIALECT_ALLOW", "1")
	t.Setenv("CODEAUDIT_MOCK_SEED", "7") // 确定性
	t.Setenv("CODEAUDIT_MOCK_DELAY_MS", "")
	r := NewManagerRunner(Config{Mode: "openshell", Agent: d})
	if events != nil {
		r.cfg.EventFn = func(level, msg string) { *events = append(*events, level+": "+msg) }
	}
	return r
}

// 一阶段（结构化通道）：mock 条目纯配置即拉起，findings 经 ADR-184 工具参数面解析。
func TestRun_MockDescriptor_Phase1Structured(t *testing.T) {
	var events []string
	r := mockRunner(t, mockDescriptor("mock", Capabilities{ToolEvents: true, StructuredSubmit: true}), &events)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "structured")
	t.Setenv("CODEAUDIT_MOCK_FINDINGS", "3")
	res, err := r.Run(context.Background(), Task{TaskID: "gw-mock1", WorkspaceDir: t.TempDir(), Assignment: "审计它"})
	if err != nil || !res.OK {
		t.Fatalf("mock run failed: %v %+v", err, res)
	}
	if len(res.Findings) != 3 {
		t.Fatalf("findings = %d, want 3（纯条目数据驱动零改动）", len(res.Findings))
	}
	if res.Meta.ResultSource != "structured" {
		t.Fatalf("result source = %q, want structured", res.Meta.ResultSource)
	}
	if !strings.HasPrefix(res.Meta.SubagentID, "mock-") {
		t.Fatalf("subagent id = %q, want mock- 前缀", res.Meta.SubagentID)
	}
}

// 补丁回合：phase-2 prompt 内嵌清单 → mock 按 index 回补 → parsePatchFixResult。
func TestRun_MockDescriptor_PatchRound(t *testing.T) {
	r := mockRunner(t, mockDescriptor("mock", Capabilities{StructuredSubmit: true}), nil)
	manifest := `[{"index":0,"title":"a","file_path":"src/mock.go","start_line":2,"fix_suggestion":"x"},{"index":1,"title":"b","file_path":"src/mock.go","start_line":5,"fix_suggestion":"y"}]`
	res, err := r.Run(context.Background(), Task{
		TaskID: "gw-mock2", WorkspaceDir: t.TempDir(), PatchFixRound: true,
		Assignment: BuildPatchPhasePrompt(manifest),
	})
	if err != nil || !res.OK {
		t.Fatalf("mock patch round failed: %v %+v", err, res)
	}
	if len(res.Patches) != 2 {
		t.Fatalf("patches = %d, want 2（清单逐项回补）", len(res.Patches))
	}
}

// capability structured_submit=false：text-final 降级解析 + ResultSource 标注 + warn 事件
// （设计 §3 诚实降级，DoD"unsupported 路径有测试"）。
func TestRun_MockDescriptor_TextFinalDegradation(t *testing.T) {
	var events []string
	d := mockDescriptor("mock", Capabilities{}) // 全 false：未声明=保守缺省（设计 §3）
	r := mockRunner(t, d, &events)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "textfinal")
	res, err := r.Run(context.Background(), Task{TaskID: "gw-mock3", WorkspaceDir: t.TempDir(), Assignment: "审计它"})
	if err != nil || !res.OK {
		t.Fatalf("text-final run failed: %v %+v", err, res)
	}
	if len(res.Findings) != mockFindingsDefault {
		t.Fatalf("fence findings = %d, want %d", len(res.Findings), mockFindingsDefault)
	}
	if res.Meta.ResultSource != "text-final" {
		t.Fatalf("result source = %q, want text-final", res.Meta.ResultSource)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "structured_submit=false") || !strings.Contains(joined, "text-final") {
		t.Fatalf("degradation events missing: %v", joined)
	}
}

// capability interaction=false：方言面冒出交互请求 → documented 行为记日志不重试，
// 收敛不受阻（DoD"interaction 拒绝的日志语义"）。
func TestRun_MockDescriptor_InteractionGated(t *testing.T) {
	var events []string
	d := mockDescriptor("mock", Capabilities{StructuredSubmit: true}) // interaction 未声明=false
	r := mockRunner(t, d, &events)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "interaction")
	res, err := r.Run(context.Background(), Task{TaskID: "gw-mock4", WorkspaceDir: t.TempDir(), Assignment: "审计它"})
	if err != nil || !res.OK {
		t.Fatalf("interaction-gated run failed: %v %+v", err, res)
	}
	if len(res.Findings) == 0 {
		t.Fatal("interaction request must not block convergence（不重试，继续收敛）")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "interaction=false") || !strings.Contains(joined, "不重试") {
		t.Fatalf("interaction gate log missing: %v", joined)
	}
}

// 未知方言在 Run 时 fail-loud（注册表无适配器）。
func TestOpenTurnSession_UnknownDialectFails(t *testing.T) {
	r := NewManagerRunner(Config{Mode: "openshell", Agent: ProviderDescriptor{ID: "x", Dialect: "carrier-pigeon"}})
	if _, err := r.openTurnSession(context.Background(), Task{}); err == nil {
		t.Fatal("unknown dialect must fail loud")
	}
}

// 收笔注入（ADR-246）：回合收敛但期望通道零提交 → 宿主注入恰一轮"只定稿不推进"，
// mock 收到"（收笔）"标记后经提交通道交齐——Cairn conclude 语义的平台化闭环。
func TestRun_MockDescriptor_ConcludeInjection(t *testing.T) {
	var events []string
	d := mockDescriptor("mock", Capabilities{StructuredSubmit: true, ConcludeSemantics: true})
	r := mockRunner(t, d, &events)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "silent")
	res, err := r.Run(context.Background(), Task{TaskID: "gw-mock5", WorkspaceDir: t.TempDir(), Assignment: "审计它"})
	if err != nil || !res.OK {
		t.Fatalf("conclude run failed: %v %+v", err, res)
	}
	if len(res.Findings) == 0 {
		t.Fatal("收笔注入后应经提交通道交齐发现")
	}
	joined := strings.Join(events, "\n")
	if got := strings.Count(joined, "收笔注入"); got != 1 {
		t.Fatalf("收笔注入次数 = %d, want 恰一轮 1（事件: %v）", got, joined)
	}
}

// capability conclude_semantics=false（Maka 面同型）：零提交收敛保持原样，不注入
// （显式 unsupported 不静默——M74 锁闸门移除）。
func TestRun_MockDescriptor_ConcludeGatedByCapability(t *testing.T) {
	var events []string
	d := mockDescriptor("mock", Capabilities{StructuredSubmit: true}) // conclude 未声明=false
	r := mockRunner(t, d, &events)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "silent")
	_, err := r.Run(context.Background(), Task{TaskID: "gw-mock6", WorkspaceDir: t.TempDir(), Assignment: "审计它"})
	// capability 关闭=silent 文本走既有解析失败路径（诚实不伪造，无收笔抢救）
	if err == nil || !strings.Contains(err.Error(), "no JSON") {
		t.Fatalf("capability 关闭时应保持既有解析失败路径, got %v", err)
	}
	if joined := strings.Join(events, "\n"); strings.Contains(joined, "收笔注入") {
		t.Fatalf("capability 关闭不得注入收笔: %v", joined)
	}
}

// 补丁回合收笔：通道感知（期望通道=submit_patches），清单在案时定稿补交。
func TestRun_MockDescriptor_ConcludePatchRound(t *testing.T) {
	d := mockDescriptor("mock", Capabilities{StructuredSubmit: true, ConcludeSemantics: true})
	r := mockRunner(t, d, nil)
	t.Setenv("CODEAUDIT_MOCK_OUTCOME", "silent")
	manifest := `[{"index":0,"title":"a","file_path":"src/mock.go","start_line":2,"fix_suggestion":"x"}]`
	res, err := r.Run(context.Background(), Task{
		TaskID: "gw-mock7", WorkspaceDir: t.TempDir(), PatchFixRound: true,
		Assignment: BuildPatchPhasePrompt(manifest),
	})
	if err != nil || !res.OK {
		t.Fatalf("patch conclude run failed: %v %+v", err, res)
	}
	if len(res.Patches) != 1 {
		t.Fatalf("收笔补交 patches = %d, want 1（通道感知 submit_patches）", len(res.Patches))
	}
}
