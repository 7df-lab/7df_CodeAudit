// 方言注册表与回合会话面抽象（R6 完整版 v0，切片 1）。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §2（方言注册表：新增方言=
// 注册表加一行+一个适配器实现）/§3（capability 消费规则）；ADR-244（接口最小集裁决）。
//
// v0 适配面 = Run 编排实际消费的五个动作：open（拉起+工作区就位）/turn/close/name/
// 观测两件（轮次累计、错误尾迹）。bridgeSession/sseParser 仍属 dsh-sse 适配器内部
// 实现（设计 §2"流：text/tool 全事件"行的实现细节），不作跨方言接口。
package sandbox

import (
	"context"
	"fmt"
	"strings"
)

// turnSession — 一次回合会话的方言无关面（Run 编排消费）。
type turnSession interface {
	// turn — 提交一条 prompt 并收敛（语义契约同 liveSession.turn：断流续跑/子任务
	// 恢复由各适配器按方言 documented 行为实现）。
	turn(ctx context.Context, prompt string) (finalText string, calls []ToolCall, err error)
	// close — 会话收束（dsh=teardown 恒回收；mock=无资源）。幂等。
	close()
	// name — 会话/沙箱标识（Result.Meta.SubagentID）。
	name() string
	// turnsContribution — 本会话已见最大轮号（R98 轮次跨回合连续化）。
	turnsContribution() float64
	// errorTail — 回合失败时的诊断尾迹（dsh=bridge.log tail；mock=空）。
	errorTail(ctx context.Context) string
}

// dialectRegistry — 方言注册表（设计 §2）：dialect → 适配器构造。新增方言=加一行+实现。
var dialectRegistry = map[string]func(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error){
	"dsh-sse": func(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) {
		return r.openDSHSession(ctx, t)
	},
	"mock-stdio": func(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) { return newMockSession(r, t) },
	"maka-runtime-host": func(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) {
		return newMakaSession(ctx, r, t)
	},
	// acp-stdio：管道式回合驱动 v0——
	// 协议面完整 ACP（initialize/session/new/prompt/update+权限代答），回合粒度、
	// 每回合新会话；逐字流+持久会话=v1 bridge 收敛批（架构 §7 既定方向）。
	"acp-stdio": func(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) {
		return newACPSession(ctx, r, t)
	},
}

// openTurnSession — 按 descriptor 方言打开回合会话（Run 编排唯一会话入口）。
func (r *ManagerRunner) openTurnSession(ctx context.Context, t Task) (turnSession, error) {
	mk, ok := dialectRegistry[r.cfg.Agent.Dialect]
	if !ok {
		return nil, fmt.Errorf("方言 %q 无适配器（注册表: dsh-sse/mock-stdio/maka-runtime-host/acp-stdio）", r.cfg.Agent.Dialect)
	}
	return mk(ctx, r, t)
}

// openDSHSession — dsh-sse 适配器：launch（descriptor 拉起面）→ 工作区上传 → 回合会话。
// 序列与拆解前的 Run 内联路径逐字等价（golden 锁：TestRun_FullLifecycleViaBridge 等）。
func (r *ManagerRunner) openDSHSession(ctx context.Context, t Task) (turnSession, error) {
	ls, err := r.launch(ctx, t.TaskID)
	if err != nil {
		return nil, err
	}
	ls.patchRound = t.PatchFixRound // ADR-243：断流续跑指令按产出通道分岔
	if t.WorkspaceDir != "" {
		if err := r.uploadProject(ctx, ls, t.WorkspaceDir); err != nil {
			ls.teardown()
			return nil, fmt.Errorf("upload project: %w", err)
		}
	}
	return dshTurnSession{ls: ls, r: r}, nil
}

// dshTurnSession — liveSession 的接口适配（零行为包装）。
type dshTurnSession struct {
	ls *liveSession
	r  *ManagerRunner
}

func (s dshTurnSession) turn(ctx context.Context, prompt string) (string, []ToolCall, error) {
	return s.ls.turn(ctx, prompt)
}
func (s dshTurnSession) close() { s.ls.teardown() }
func (s dshTurnSession) name() string {
	if s.ls.ref.Name != "" {
		return s.ls.ref.Name
	}
	return s.ls.ref.ID
}
func (s dshTurnSession) turnsContribution() float64 { return s.ls.maxTurn }
func (s dshTurnSession) errorTail(ctx context.Context) string {
	return s.r.bridgeLogTail(ctx, s.ls.url, s.ls.token, s.ls.ref.ID)
}

// agentCaps — 选中条目的 capability profile（nil 安全缺省）。
func (r *ManagerRunner) agentCaps() Capabilities { return r.cfg.Agent.Capabilities }

// （agentBridgeCmd/agentPermissionMode 见 session.go——拉起面与 launchScript 同址。）

// textFinalParse — structured_submit=false 的结果解析（设计 §3：text-final + engine 侧
// 兜底解析，语义标注降级来源）。复用 ADR-184 解析器的围栏降级通道（工具调用参数为空
// 即自然落到 ```json 兜底），不另写第二套 JSON 提取。
func textFinalParse(finalText string, patchRound bool) ([]Finding, []PatchFix, error) {
	if patchRound {
		patches, _, err := parsePatchFixResult(finalText, nil)
		return nil, patches, err
	}
	findings, _, err := parseAuditResult(finalText, nil)
	return findings, nil, err
}

// resultSourceOf — 解析来源标注值（诚实降级：来源进 Result.Meta 与任务日志）。
func resultSourceOf(d ProviderDescriptor) string {
	if d.Capabilities.StructuredSubmit {
		return "structured"
	}
	return "text-final"
}

// initialPromptOf — seeds.render_target 消费（设计 §2）：task-card-prefix=任务卡前置段
// （现行 buildTurnPrompt 嵌任务）；initial-prompt=assignment 即初始 prompt 直发（Maka
// 面预留，v0 校验过未消费——首个 initial-prompt 条目接入时此处即分派点）。
func initialPromptOf(d ProviderDescriptor, t Task) string {
	if d.Seeds.RenderTarget == "initial-prompt" {
		return strings.TrimSpace(t.Assignment)
	}
	return ""
}
