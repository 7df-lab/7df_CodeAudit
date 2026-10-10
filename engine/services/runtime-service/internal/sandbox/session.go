// 沙箱多轮会话与项目上传：
//   - 4a VerifySASTResults：整项目代码上传进沙箱磁盘，逐条把"文件/行/CWE/疑点"提交为
//     独立 prompt 由 DSH 自行阅读源码判定真伪，逐条取回最终结论，全部完成后再销毁沙箱；
//   - 4b SearchMissedVulns：整项目上传后一次 prompt 要求 DSH 全项目审计并返回发现 JSON。
//
// 项目上传走 openshell-manager `POST /api/v1/sandboxes/{id}/files`（multipart，path+file；
// 流式转发、.part 原子落盘）——本执行器打 tar.gz 单请求上传，沙箱内解包到 /sandbox/project。
package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProjectSandboxPath — 项目在沙箱内的解包根（policy read_write 含 /sandbox，DSH 可读）。
// 导出供 service 层在 prompt 中告知 DSH 项目位置（ADR-173）。
const ProjectSandboxPath = "/sandbox/project"

// effectiveProjectPath — 切片 7：agent 侧项目落位（dsh=/sandbox/project 既有；
// 非 dsh-sse 方言=/workspace/project——镜像 USER 可写位）。
func effectiveProjectPath(cfg Config) string {
	if cfg.Agent.Dialect != "dsh-sse" {
		return "/workspace/project"
	}
	return ProjectSandboxPath
}

// maxProjectArchiveBytes — 上传压缩包上限（与网关用户上传 100MB 同量级；超限如实报错）。
const maxProjectArchiveBytes = 100 << 20

// SessionTask — 一次多轮沙箱会话（ADR-173）。
type SessionTask struct {
	TaskID     string        // 任务号（沙箱命名/环境变量）
	ProjectDir string        // 本地项目代码目录（空=不上传，复用沙箱既有磁盘内容）
	Prompts    []string      // 逐条提交的任务（4a=N 条逐条审查；4b=1 条整审）
	Timeout    time.Duration // 整会话上限（07 §8 OpenShell 沙箱执行 30m 内取用）
}

// RunSession — 完整生命周期 + 项目上传 + 逐条 prompt 会话；返回每条 prompt 的
// DSH 最终 assistant 消息（顺序对应 Prompts）。任何一步失败即 teardown 并报错。
func (r *ManagerRunner) RunSession(ctx context.Context, t SessionTask) ([]string, error) {
	if !r.Enabled() {
		return nil, ErrDisabled
	}
	// R76: 整会话上限接线（07 §8:126；原 SessionTask.Timeout 死字段同 Run）
	ctx, cancel := withDeadline(ctx, t.Timeout)
	defer cancel()
	ls, err := r.launch(ctx, t.TaskID)
	if err != nil {
		return nil, err
	}
	defer ls.teardown()
	defer ClearSession(t.TaskID) // ADR-200: 会话结束清闸门（防注册表泄漏）

	if t.ProjectDir != "" {
		if err := r.uploadProject(ctx, ls, t.ProjectDir); err != nil {
			return nil, fmt.Errorf("upload project: %w", err)
		}
	}

	// ADR-200: 会话闸门——PauseTask 后在回合边界挂起（当前回合跑完生效），
	// ResumeTask 后继续；ctx 取消（取消/超时）时如实报错。
	// 寻址走 gateKey（S2 修复）：-fix 派生回合共享主任务闸门与冻结锚。
	gate := GateFor(gateKey(t.TaskID))

	finals := make([]string, 0, len(t.Prompts))
	for i, prompt := range t.Prompts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// ADR-200 闸门 + R2 冻结回调（TP17-T2）：进入挂起时先锚换手基线
		// （重投影+checkpointed——此刻回合已收束、SSE 已定格，Run 收束的 defer
		// 投影对中途冻结不生效）。未暂停路径零开销（同 gate.Wait）。
		// R4（切片 1）：挂起中 ctx 取消（换手接管）= handoff hold 登记保活——
		// defer teardown 据此跳过 DELETE，窗口后对账回收。
		if err := gate.WaitFrozen(ctx, gateKey(t.TaskID)); err != nil {
			HoldSandbox(ls.ref.Name, HoldTTL())
			r.event("info", "换手窗口：沙箱 %s 保活 %v（R4 handoff hold）", ls.ref.Name, HoldTTL())
			return nil, err
		}
		r.event("info", "会话轮 %d/%d 提交（prompt %d 字节）", i+1, len(t.Prompts), len(prompt))
		text, _, err := ls.turn(ctx, prompt)
		if err != nil {
			return nil, fmt.Errorf("turn %d/%d: %w", i+1, len(t.Prompts), err)
		}
		r.event("info", "会话轮 %d/%d 收敛（最终消息 %d 字节）", i+1, len(t.Prompts), len(text))
		finals = append(finals, text)
	}
	return finals, nil
}

// agentBridgeCmd — bridge 拉起命令解析（R6 v0/切片 1）：descriptor 条目值 > legacy
// cfg 覆盖（R6 首批双轨键，收口随 descriptor 验收）> 默认 DSH bridge 常量。
func (r *ManagerRunner) agentBridgeCmd() string {
	if v := r.cfg.Agent.Launch.BridgeCmd; v != "" {
		return v
	}
	if r.cfg.AgentBridgeCmd != "" {
		return r.cfg.AgentBridgeCmd
	}
	return bridgeCmd
}

// agentPermissionMode — 拉起面权限模式（R6 v0：descriptor 条目值 > danger-full-access
// 缺省（ADR-187 补遗：内层全放行=设计模式，架构 §3）。
func (r *ManagerRunner) agentPermissionMode() string {
	if v := r.cfg.Agent.Launch.PermissionMode; v != "" {
		return v
	}
	return "danger-full-access"
}

// liveSession — 已拉起 bridge 并完成服务暴露的活动会话（launch→turn…→teardown）。
type liveSession struct {
	r            *ManagerRunner
	url, token   string
	ref          sandboxRef
	sess         *bridgeSession
	events       <-chan bridgeEvent
	streamErr    <-chan error
	cancelStream context.CancelFunc
	torn         bool    // teardown 幂等闸
	maxTurn      float64 // 本会话已见最大轮号（R98：turn 事件循环更新，Run 收束后累加进 r.turnsUsed）
	patchRound   bool    // 补丁回合标记（ADR-243：断流续跑指令按产出通道分岔）

	// 切片 5+6（ADR-249，审计 H1）：图执行焦点会话与分会话轮账。focus 由 runner 在
	// 节点回合边界切换（prompt 目标+parser 口径同步）；nodeTurns=各节点会话各自
	// max 轮号（turnsUsed 统计归集用——跨会话 max 合并会少计，H1 轮账分账）。
	focusMu   sync.Mutex
	focus     string
	nodeTurns map[string]float64
}

// setFocus — 设定图执行焦点会话（H1）：liveSession 侧 prompt 寻址与 parser 侧
// 渲染/收敛口径同步切换。空=回主会话（现网审计路径口径）。
func (ls *liveSession) setFocus(sid string) {
	ls.focusMu.Lock()
	ls.focus = sid
	ls.focusMu.Unlock()
	if ls.sess != nil && ls.sess.parser != nil {
		ls.sess.parser.setFocus(sid)
	}
}

// currentFocus — 当前焦点会话（空=main）。
func (ls *liveSession) currentFocus() string {
	ls.focusMu.Lock()
	defer ls.focusMu.Unlock()
	return ls.focus
}

// nodeTurnsFor — 指定节点会话的累计轮次（图模式 usageEstimate 面）。
func (ls *liveSession) nodeTurnsFor(sid string) float64 {
	ls.focusMu.Lock()
	defer ls.focusMu.Unlock()
	return ls.nodeTurns[sid]
}

// focusTurnsTotal — 图模式全节点会话轮次合计（usage 观测归集；main 会话不计——
// 图模式 main 无回合）。
func (ls *liveSession) focusTurnsTotal() float64 {
	ls.focusMu.Lock()
	defer ls.focusMu.Unlock()
	var total float64
	for _, v := range ls.nodeTurns {
		total += v
	}
	return total
}

// patchRoundPrompt — 断流续跑指令按回合类型分岔：补丁回合的产出通道是 submit_patches。
func (ls *liveSession) patchRoundPrompt() string {
	if ls.patchRound {
		return continueAfterStreamBreakPatches
	}
	return continueAfterStreamBreak
}

// launch — create → wait-ready → exec 拉起 bridge → ExposeService → SSE 订阅。
// 创建成功后的任何失败都会先 teardown 再返回（沙箱不泄漏）；调用方随后必须 defer teardown。
func (r *ManagerRunner) launch(ctx context.Context, taskID string) (ls *liveSession, err error) {
	url, token := r.managerEndpoint()
	waitTimeout := time.Duration(r.cfg.WaitReadyTimeoutS) * time.Second // 07 §8

	var created sandboxRef
	// create→wait-ready 整体最多两轮：docker 驱动偶发 ContainerExited 会让
	// wait-ready 一次失败即触发降级链过脆；每轮失败先回收本轮沙箱再重来。
	// M1（provider attachment 设计）：循环外读一次路由快照——重试轮不重复打
	// manager，且挂载（spec.providers）与 env 分派（llmRouteEnvFromRoute）共用同一
	// 快照（code-review P2#2：消除双读撕裂窗口——切路由瞬间的在途 launch 拿到
	// "合成层=旧、直连 env=新"会被 fail-closed 拒）。挂载渐进激活（无 profile 的
	// provider 网关侧跳过合成层，零行为变化）。
	routeSnap := r.inferenceRouteSnapshot(ctx)
	for attempt := 1; ; attempt++ {
		name := "ca-" + randomHex12() // 15 字符 ≤ 网关沙箱服务路由 19 上限（manager ExposeService 实测报错口径）
		// ADR-210: 前缀 am-→ca-（auditmind→codeaudit；对账正则 ^(am|ca)- 兼容存量旧名）。
		// 进程级活跃注册表：launch 注册于创建动作之前（防对账竞态），teardown 注销。
		activeSandboxes.Store(name, struct{}{})
		r.event("info", "沙箱创建中 name=%s image=%s workspace=%s（第 %d 次）", name, effectiveImage(r.cfg), r.cfg.Workspace, attempt)
		spec, perr := sandboxSpecWithPolicy(name, taskID, r.cfg)
		if perr != nil {
			// ADR-248：targeted 组合失败 fail-closed——模板缺失/形状断言失败即
			// launch 失败（任务可见），不静默回落 restricted（D1/模板头注明文）。
			activeSandboxes.Delete(name)
			r.event("error", "targeted policy 组合失败（fail-closed）: %v", perr)
			return nil, fmt.Errorf("compose targeted policy: %w", perr)
		}
		if spec != nil && spec["policy"] != nil {
			// C3 任务级留痕：组合时模板内容锚（审计可对账"用的哪个模板版本"）
			r.event("info", "targeted spec.policy 注入 endpoints=%d 模板 sha256=%s", len(r.cfg.AuthorizedTargets), spec["_policy_digest"])
			delete(spec, "_policy_digest") // 内部审计位，不进 manager 请求体
		}
		// M1：spec.providers 挂载（sandboxSpec 缺省空数组之上填路由 provider）——
		// 网关按 platform-scope profile 合成 _provider_<名> 端点规则+占位符凭据快照。
		if routeSnap != nil && routeSnap.Provider != "" {
			spec["providers"] = []string{routeSnap.Provider}
		}
		ref, cerr := r.call(ctx, "POST", url+"/api/v1/sandboxes", token, map[string]any{
			"workspace": r.cfg.Workspace, "name": name, "spec": spec,
		})
		if cerr != nil {
			// ADR-212: 注册先于创建（防对账竞态），失败必须注销——否则每次失败泄
			// 一条注册表记录；最坏情形（manager 实际已建但响应丢失）真孤儿被本进程
			// 注册表永久屏蔽，对账器（ADR-210）永不回收。
			activeSandboxes.Delete(name)
			r.event("error", "沙箱创建失败（第 %d 次）: %v", attempt, cerr)
			if attempt < 2 {
				continue
			}
			return nil, fmt.Errorf("create sandbox: %w", cerr)
		}
		created = sandboxRefFrom(ref)
		r.event("info", "沙箱已创建 id=%s name=%s phase=%s", created.ID, created.Name, created.PhaseName)

		ls = &liveSession{r: r, url: url, token: token, ref: created}
		if _, werr := r.call(ctx, "POST", url+"/api/v1/sandboxes/"+created.Name+"/wait-ready", token,
			map[string]any{"workspace": r.cfg.Workspace, "timeout_seconds": int(waitTimeout.Seconds())}); werr != nil {
			r.event("error", "沙箱等待就绪失败（第 %d 次）: %v", attempt, werr)
			ls.teardown() // 回收本轮沙箱（含注册表注销）后重试/放弃
			ls = nil
			if attempt < 2 {
				continue
			}
			return nil, fmt.Errorf("wait-ready: %w", werr)
		}
		break
	}
	r.event("info", "沙箱就绪（wait-ready 通过）")
	// 循环内失败路径已自行 teardown；此处起恒 teardown 纪律（ls 为 nil 时未创建无需回收）
	defer func() {
		if err != nil && ls != nil {
			ls.teardown()
		}
	}()

	// 切片 7：非 dsh-sse 方言（maka-runtime-host 等）无 bridge 通道——launch 到
	// 沙箱就绪为止（open 面的方言特定初始化由各适配器自理，如 maka 健康面）。
	if r.cfg.Agent.Dialect != "dsh-sse" {
		r.event("info", "方言 %s 无 bridge 通道——launch 止于沙箱就绪", r.cfg.Agent.Dialect)
		return
	}
	// exec 拉起 bridge（openshell supervisor 覆盖镜像 ENTRYPOINT，bridge 不自启——
	// CD/dsh-pentest-sse/README 已知接线点）。凭据为占位符：网关按引用注入真凭据。
	// DSH_PERMISSION_MODE=danger-full-access（ADR-187 补遗）：DSH 内层 bash 收禁
	// （bubblewrap/Landlock backend）在 openshell 容器内不可用，默认 workspace-write
	// 下 bash 工具全断（模型被迫 fallback 文件工具，pdtools/nuclei/sqlmap 工具链全废，
	// 升级请求 approval/asked 在 headless 下无人审批）。confinement 边界本就是
	// openshell policy（filesystem/egress/run_as_user），DSH 内层跳过不越界——
	// env 覆盖是 DSH 官方入口（bundle base cordis.patch.yml sandbox-policy.mode）。
	// LLM env 段按当前推理路由分派（ADR-227）：anthropic 型 → anthropic-relay
	// （bridge 烘 llm-pi-ai 路由打 L7 /v1/messages）；其余 → deepseek 适配器。
	launchScript := fmt.Sprintf(
		"nohup env DSH_MAX_TOKENS=%d DSH_PERMISSION_MODE=%s %s %s > %s 2>&1 & "+
			"for i in $(seq 1 15); do curl -s -m 2 %s | grep -q '\"ok\":true' && exit 0; sleep 1; done; "+
			"echo bridge-not-ready; tail -5 %s; exit 1",
		r.cfg.DSHMaxTokens, r.agentPermissionMode(), r.llmRouteEnvFromRoute(ctx, routeSnap), r.agentBridgeCmd(), bridgeLogPath, bridgeHealthz, bridgeLogPath)
	if _, err = r.execIn(ctx, url, token, created.ID, launchScript, 60); err != nil {
		r.event("error", "拉起 bridge 失败: %v", err)
		err = fmt.Errorf("launch bridge: %w", err)
		return
	}
	r.event("info", "bridge 已就绪（沙箱内 healthz 通过，maxTokens=%d）", r.cfg.DSHMaxTokens)

	// ExposeService：沙箱内 8080 → 网关服务路由 URL（路径参数是沙箱名，非 UUID）。
	exposed, err2 := r.call(ctx, "POST", url+"/api/v1/sandboxes/"+created.Name+"/services", token, map[string]any{
		"workspace": r.cfg.Workspace, "service": bridgeServiceName, "target_port": bridgeTargetPort,
	})
	if err2 != nil {
		r.event("error", "暴露 bridge 服务失败: %v", err2)
		err = fmt.Errorf("expose bridge: %w", err2)
		return
	}
	svcURL, _ := exposed["url"].(string)
	if svcURL == "" {
		r.event("error", "暴露响应缺少 url: %v", exposed)
		err = fmt.Errorf("expose bridge: missing url")
		return
	}
	r.event("info", "bridge 已暴露 %s", svcURL)

	ls.sess = &bridgeSession{svcURL: svcURL, dial: r.dialAddr(url), hc: r.hc, turnBase: r.turnsUsed, phaseTag: r.phaseTag}

	// 先订阅 SSE 再 prompt（不丢早期事件）；订阅流全程原样外流。
	streamCtx, cancelStream := context.WithCancel(ctx)
	ls.cancelStream = cancelStream
	events := make(chan bridgeEvent, 256)
	streamErr := make(chan error, 1)
	go func() {
		// ADR-210: SSE 解析 panic 不得带崩进程——否则本进程全部活沙箱因 defer 不再执行而齐漏
		defer func() {
			if rec := recover(); rec != nil {
				streamErr <- fmt.Errorf("sse stream panic(recovered): %v", rec)
			}
		}()
		streamErr <- ls.sess.stream(streamCtx, r.rawLog, r.humanLog, r.channelLog, events)
	}()
	time.Sleep(300 * time.Millisecond) // 给订阅留出握手窗口（bridge.hello 回显）
	ls.events = events
	ls.streamErr = streamErr
	return ls, nil
}

// recoverAgentPrompt — 子任务死亡后的恢复指令（ADR-190，恰一轮）：主会话在等
// 的回报永不到达，驱动模型基于已有分析立即提交（fixretry 失败反馈自纠同精神）。
const recoverAgentPrompt = "你此前派发的后台子任务因推理流中断而失败，其结果不会再返回。" +
	"请基于已掌握的分析结果继续：若信息足够，立即按输出契约调用 submit_findings 提交全部发现；" +
	"若关键信息缺失，重新派发子任务或自行完成该部分分析后再提交。"

// continueAfterStreamBreak — 主会话推理流瞬态中断后的继续指令（ADR-192，≤2 轮）：
// 会话历史仍在沙箱内存中，模型可见自己被截断的提交，直接重发（Cline 对 API 流断
// 的回合级重试映射）。ADR-194 分批提交：断流可能发生在批间——已成功的批次在服务
// 端已累积，续跑只需补齐剩余批次。ADR-211 自适应缩批：既然上一批把流养断了，续跑
// 再按原批量重试就是重蹈覆辙——指令显式要求后续每批进一步缩小（含补丁每批 1 条），
// 逐批连续提交、在服务端累积（turn() 跨回合合并），单批越小落袋概率越高。
const continueAfterStreamBreak = "你上一回合的推理流在输出中途被截断（内容未送达）。" +
	"请继续：若此前的 submit_findings 已有批次成功提交，只需提交剩余批次；若分析已完成但尚未开始提交，" +
	"立即按输出契约分批提交全部发现。截断说明单批参数流过长：后续每批进一步缩小——" +
	"含 diff_patch 的发现每批 1 条、上下文行每侧最多 3 行，逐批连续提交（服务端合并去重）；" +
	"不要重复已完成的探索。"

// continueAfterStreamBreakPatches — 补丁回合（PatchFixRound，ADR-241/243）的续跑指令：
// 产出通道是 submit_patches——审计版续跑指令会把模型往 findings 通道引（契约错位，
// 对抗审查 R97 补遗发现；混入的 findings 调用会被服务端按契约忽略=白烧 token）。
const continueAfterStreamBreakPatches = "你上一回合的推理流在输出中途被截断（内容未送达）。" +
	"请继续：若此前的 submit_patches 已有批次成功提交，只需提交剩余批次；若补丁已全部生成但尚未开始提交，" +
	"立即按输出契约分批提交全部补丁。截断说明单批参数流过长：后续每批进一步缩小——" +
	"单个补丁单独成批，逐批连续提交（服务端按 index 合并）；" +
	"不要重复已完成的探索。"

// maxTurnStreamRetries — 主会话瞬态流断的回合级重试上限（ADR-192）。
const maxTurnStreamRetries = 2

// isTransientStreamErr — 瞬态流断判定（值得续跑重试）：provider/网络层的流中断族，
// 以及流中途数据级损坏（malformed SSE payload，ADR-227）——后者与连接级断流同族：
// 请求本身合法、会话历史完好，续跑指令让模型重发提交即可恢复；重发整个模型请求的
// 提供商级重试才有增量重复顾虑（dsh-retry-policy 对 STREAM_CLOSED 的排除理由），
// 回合级续跑不重发请求、模型基于历史重新生成，无此问题。
// 非瞬态（参数非法/鉴权/模型不存在等）重试无意义，立即失败。
func isTransientStreamErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{
		"SSE stream ended", "STREAM_CLOSED", "stream disconnected",
		"connection reset", "connection refused", "broken pipe",
		"i/o timeout", "context deadline", "EOF", "unavailable", "overloaded",
		// "malformed SSE payload:" 前缀是 dsh llm-deepseek translate 的稳定消息
		// 形态（gw-a00057d9691e0097f1b546da 实证；跨仓前缀契约同 ADR-192 补遗
		// 对 "SSE stream ended without [DONE]" 的处理）
		"malformed SSE payload",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// turn — 提交一条 prompt 并等待本回合收敛，返回最终 assistant 文本与本回合捕获的
// 工具调用（ADR-184：submit_findings/submit_patches 等结构化提交经此透出）。
//
// 收敛纪律：
//   - ADR-190：idle 只认主会话；主会话 idle 时若存在已死子任务且尚未提交 → 发恢复
//     指令恰一轮；再 idle 如实收敛（调用方走失败/降级链，绝不循环）；
//   - ADR-192：主会话推理流瞬态中断（STREAM_CLOSED 等）→ 发继续指令重试 ≤2 轮
//     （会话历史在，模型重发提交即可；gw-d911757 实证：7 项发现确认完毕、死于
//     submit_findings 参数流式生成途中，全部作废）；错误回合的收尾 idle 不当收敛。
func (ls *liveSession) turn(ctx context.Context, prompt string) (string, []ToolCall, error) {
	if err := ls.sess.prompt(ctx, ls.currentFocus(), prompt); err != nil {
		return "", nil, fmt.Errorf("prompt: %w", err)
	}
	final := ""
	var calls []ToolCall
	deadAgents := map[string]bool{}
	rescued := false
	retries := 0
	retryPending := false // 已发继续指令、等待错误回合的收尾 idle 与新回合
	hasSubmit := func() bool {
		for _, c := range calls {
			if c.Name == SubmitFindingsTool {
				return true
			}
		}
		return false
	}
	for {
		select {
		case <-ctx.Done():
			if hasSubmit() {
				return final, calls, nil // 产出已在手：交回而非丢弃（取消≠失败）
			}
			return "", nil, ctx.Err()
		case err := <-ls.streamErr:
			return "", nil, fmt.Errorf("event stream: %w", err)
		case ev, ok := <-ls.events:
			if !ok {
				return "", nil, fmt.Errorf("event stream closed unexpectedly")
			}
			if ev.err != nil {
				// ADR-194：瞬态断流一律续跑（分批提交中途断流时 hasSubmit 也为真——
				// 剩余批次仍需补齐，已提交批次经 calls 累积不丢）；非瞬态立即失败。
				if isTransientStreamErr(ev.err) && retries < maxTurnStreamRetries {
					retries++
					retryPending = true
					hopLines, hopAge := ls.sess.HopStats()
					ls.r.event("warn", "主会话推理流瞬态中断（%v），发继续指令重试（%d/%d，ADR-192）"+
						"——hop 归因: bridge 流累计 %d 行，末帧距今 %s（停滞≈沙箱→dsh 跳断；正常≈DSH→上游断，诊断见错误消息）",
						ev.err, retries, maxTurnStreamRetries, hopLines, hopAge)
					if err := ls.sess.prompt(ctx, ls.currentFocus(), ls.patchRoundPrompt()); err != nil {
						return "", nil, fmt.Errorf("continue prompt: %w", err)
					}
					continue
				}
				if hasSubmit() {
					return final, calls, nil // 回合报错但产出已在手（保险）
				}
				return "", nil, fmt.Errorf("dsh turn error: %w", ev.err)
			}
			if ev.agentErr != "" {
				deadAgents[ev.agentErr] = true // 子任务死亡：不失败，等主会话处置
				continue
			}
			if ev.turnStart {
				if sid := ls.currentFocus(); sid != "" {
					// H1：图模式轮次归焦点节点会话（跨会话 max 合并会少计）
					if ls.nodeTurns == nil {
						ls.nodeTurns = map[string]float64{}
					}
					if ev.turnNo > ls.nodeTurns[sid] {
						ls.nodeTurns[sid] = ev.turnNo
					}
				} else if ev.turnNo > ls.maxTurn {
					ls.maxTurn = ev.turnNo
				}
				final = "" // 新回合开始：只认本回合的最终消息
				// ADR-194：提交类工具调用跨回合累积（分批提交+断流重试——已成功批次
				// 不因新回合重置丢失；重复批次由 parseAuditResult 去重吸收）
				var kept []ToolCall
				for _, c := range calls {
					if c.Name == SubmitFindingsTool || c.Name == SubmitPatchesTool {
						kept = append(kept, c)
					}
				}
				calls = kept
				retryPending = false
			}
			if ev.assistantText != "" {
				final = ev.assistantText
			}
			if ev.toolCall != nil {
				calls = append(calls, *ev.toolCall)
			}
			if ev.idle {
				if retryPending {
					retryPending = false // 错误回合的收尾 idle：继续指令已发，等新回合
					continue
				}
				if len(deadAgents) > 0 && !rescued && !hasSubmit() {
					rescued = true
					deadAgents = map[string]bool{}
					ls.r.event("warn", "子任务推理流中断，向主会话发恢复指令（恰一轮，ADR-190）")
					if err := ls.sess.prompt(ctx, ls.currentFocus(), recoverAgentPrompt); err != nil {
						return "", nil, fmt.Errorf("recover prompt: %w", err)
					}
					continue // 恢复指令触发新回合：继续等收敛
				}
				return final, calls, nil
			}
		}
	}
}

// teardown — 取消 SSE 订阅并删除沙箱（恒 teardown，幂等；删除失败不掩盖主流程错误）。
// R4（切片 1）：handoff hold 有效期内跳过 DELETE——换手窗口免重解包（正确性由 R3 快照
// 保证）；仍注销活跃注册表，TTL 后由 SandboxReconciler 按孤儿纪律回收。
func (ls *liveSession) teardown() {
	if ls.torn {
		return
	}
	ls.torn = true
	if ls.cancelStream != nil {
		ls.cancelStream()
	}
	if sandboxHeld(ls.ref.Name) {
		ls.r.event("info", "handoff hold：跳过沙箱回收 name=%s（换手窗口保活，R4；%v 后对账回收）", ls.ref.Name, HoldTTL())
		activeSandboxes.Delete(ls.ref.Name)
		return
	}
	del := func() error {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := ls.r.call(delCtx, "DELETE",
			ls.url+"/api/v1/sandboxes/"+ls.ref.Name+"?workspace="+neturl.QueryEscape(ls.r.cfg.Workspace), ls.token, nil) // ADR-212: workspace 转义（对齐 uploadFile）
		return err
	}
	// ADR-210: 旧实现吞错且无条件打"已回收"（manager 瞬时不可达一次即永久泄漏，日志与事实相悖）
	// ——改为按 DELETE 结果如实输出，失败重试一次；仍失败者留给 SandboxReconciler 对账回收。
	err := del()
	if err != nil {
		ls.r.event("error", "沙箱回收失败将重试 name=%s err=%v", ls.ref.Name, err)
		time.Sleep(3 * time.Second)
		err = del()
	}
	if err != nil {
		ls.r.event("error", "沙箱回收仍失败（留给对账回收） name=%s err=%v", ls.ref.Name, err)
	} else {
		ls.r.event("info", "沙箱已回收 name=%s", ls.ref.Name)
	}
	// 无论成败都注销：失败者转入"非活跃"，由 SandboxReconciler 下轮重删
	activeSandboxes.Delete(ls.ref.Name)
}

// uploadProject — 项目目录打 tar.gz → manager files 接口单请求上传 → 沙箱内解包。
// 排除 walkExcludes 目录（依赖/构建产物不进沙箱）；超限如实报错，不静默截断。
func (r *ManagerRunner) uploadProject(ctx context.Context, ls *liveSession, projectDir string) error {
	// 切片 7：解包目标按方言（dsh=/sandbox/project；maka 镜像 USER=node，
	// /workspace 可写——mkdir /sandbox 会 Permission denied，sim gw-14635dc 实证）。
	PROJECT_DST := effectiveProjectPath(r.cfg)
	started := time.Now()
	gz, n, err := tarProject(projectDir)
	if err != nil {
		return err
	}
	r.event("info", "项目打包完成 %s（%d 字节）→ 上传沙箱", projectDir, n)

	// manager files 接口：multipart/form-data（JSON 415；必须带 Content-Length 否则 411）。
	// 路径参数按 README 为沙箱 {id}；manager 各端点 id/name 混用（exec=UUID、services=name），
	// 名称优先、404 回退 UUID——契约以实测为准。
	archivePath := "/tmp/am-project.tar.gz"
	// ADR-174：manager 接口层收 name、内部自解析 UUID；
	// 执行器统一以 name+workspace 寻址（与其余沙箱端点一致）。
	if err := r.uploadFile(ctx, ls.url, ls.token, ls.ref.Name, archivePath, gz); err != nil {
		return err
	}

	// 解包到 PROJECT_DST（先清残留保证内容与本次任务一致），并校验结果可见。
	extract := fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xzf %s -C %s && ls %s | head -3",
		PROJECT_DST, PROJECT_DST, archivePath, PROJECT_DST, PROJECT_DST)
	if _, err := r.execIn(ctx, ls.url, ls.token, ls.ref.ID, extract, 60); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	r.event("info", "项目已就位于沙箱 %s（耗时 %s）", PROJECT_DST, time.Since(started).Round(time.Second))
	return nil
}

// uploadFile — 单文件 multipart 上传（manager 流式转发，2MiB 分块写沙箱）。
// body 整体缓冲：manager 对该端点强制 Content-Length（chunked 411，README 明示），
// io.Pipe 这类未知长度 body 会被 Go 客户端改成分块传输——实测踩坑。
func (r *ManagerRunner) uploadFile(ctx context.Context, url, token, sandboxName, path string, content []byte) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("path", path); err != nil {
		return err
	}
	fw, err := mw.CreateFormFile("file", "am-project.tar.gz")
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, bytes.NewReader(content)); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		url+"/api/v1/sandboxes/"+sandboxName+"/files?workspace="+neturl.QueryEscape(r.cfg.Workspace),
		bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.ContentLength = int64(body.Len()) // 管理端 411 校验要求
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return fmt.Errorf("openshell-manager unreachable (POST files): %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openshell-manager POST files -> HTTP %d: %s", resp.StatusCode, tail(string(raw), 200))
	}
	return nil
}

// tarProject — 目录 → tar.gz（排除 walkExcludes；返回内容与压缩字节数）。
func tarProject(root string) ([]byte, int, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个不可读条目跳过（与 uploadProject 打包跳过语义一致，ADR-187）
		}
		if info.IsDir() {
			if walkExcludes[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || walkExcludes[info.Name()] {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// R62：流式拷贝边打边量——原实现 ReadFile 整文件入内存，
		// 超限判定在打包完成后才触发（内存已冲高）；现按缓冲区水位即时中止
		if buf.Len()+int(info.Size()) > maxProjectArchiveBytes {
			return fmt.Errorf("project tar exceeds %d bytes", maxProjectArchiveBytes)
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return nil // 单条目不可读跳过（原 ReadFile 失败同语义）
		}
		_, cerr := io.Copy(tw, f)
		f.Close()
		return cerr
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	if buf.Len() > maxProjectArchiveBytes {
		return nil, 0, fmt.Errorf("project archive %d bytes exceeds %d cap", buf.Len(), maxProjectArchiveBytes)
	}
	if buf.Len() == 0 {
		return nil, 0, fmt.Errorf("no files found in project dir %s", root)
	}
	return buf.Bytes(), buf.Len(), nil
}

// TarProjectTree — 工作区树 tar.gz（R3 tree_ref 快照导出面，切片 1；排除面/100MB 上限
// 与 uploadProject 打包同源）。依据: 主仓库 baton/docs/engine-integration-req.md R3
// （handoff.checkpointed.workspace.tree_ref 可离线解析回源码树）/ ADR-244（v0 落点=
// tasklog 工作区卷，桶引用升级随 manager API 评估）。
func TarProjectTree(root string) ([]byte, error) {
	gz, _, err := tarProject(root)
	return gz, err
}

// concludePromptFindings / concludePromptPatches — 收笔注入 prompt（ADR-246，Cairn
// conclude 语义吸收 B4：固定语义"不要继续探索，只总结截至目前已完成的探索与结论"，
// 平台化为"只定稿不推进"+通道化提交契约）。标记"（收笔）"供 mock 方言识别（测试面）。
const concludePromptFindings = "收笔（conclude）：不要继续探索，不要派发新的子任务或执行新命令。" +
	"请基于截至目前已完成的探索与结论立即定稿：把全部发现按输出契约经 submit_findings 分批提交" +
	"（此前已成功提交的批次服务端已留存，不要重复），全部提交完成后再写一句最终摘要。"

const concludePromptPatches = "收笔（conclude）：不要继续探索，不要派发新的子任务或执行新命令。" +
	"请基于目前已生成的补丁立即定稿：把全部补丁按输出契约经 submit_patches 分批提交" +
	"（此前已成功提交的批次服务端已留存，不要重复），全部提交完成后再写一句最终总结。"

// concludePrompt — 按回合类型选收笔 prompt（通道化：audit=findings / patch=patches）。
func concludePrompt(patchRound bool) string {
	if patchRound {
		return concludePromptPatches
	}
	return concludePromptFindings
}
