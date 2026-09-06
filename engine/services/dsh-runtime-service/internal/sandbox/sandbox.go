// Package sandbox — 经 openshell-manager 微服务的沙箱内 DSH 执行器（bridge 通道，ADR-168）。
//
// 架构（ADR-140 人类裁决 2026-08-29 + ADR-166 2026-08-31 + ADR-168 2026-09-01）：
// CodeAudit 的 LLM 语义分析在 OpenShell 沙箱内运行 DSH（DeepSeek Harness）。沙箱镜像
// dsh-pentest-sse 内置 bridge（JSON-RPC⇄SSE 桥接层，事实源 CD/dsh-pentest-sse/），
// 本执行器经 openshell-manager 微服务完成生命周期（create/wait-ready/exec/expose/delete，
// 唯一南向传输层），再经网关 ExposeService 路由与 bridge HTTP+SSE 直交互：
//
//	create → wait-ready → exec 拉起 bridge → ExposeService(8080)
//	→ SSE /events 订阅 → POST /prompt（审计任务）→ 会话事件流回显
//	→ session.status=idle → 提取最终 assistant 消息 → 解析 findings → 恒 teardown
//
// 交互日志双流（ADR-168 补遗）：原始 SSE 帧经 OnRawLog 落盘留存（机器调试用，不经
// RPC 面向用户）；按 event type 人性化渲染的中文流经 OnHumanLog 实时外流——这才是
// proto GetAIInteractionLog 的 chunk 内容，GUI 实时可见、任务终态即最终交互日志。
//
// LLM egress 走网关推理路由（inference.local，凭据由网关注入，永不进沙箱/prompt/命令行）。
// 诚实降级：mode=off 或 manager 不可达时，调用方回退既有 RuleScan/NEEDS_MANUAL 链（07 §10）。
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ErrDisabled — mode=off 时的哨兵错误（调用方据此走降级链，不视为故障）。
var ErrDisabled = errors.New("sandbox mode=off (dsh_runtime.sandbox.mode)")

// Task — 一次沙箱语义分析任务。
type Task struct {
	TaskID       string // 任务号（沙箱 environment.DSH_TASK_ID 与命名前缀用）
	WorkspaceDir string // 待审计代码目录（tar 整包上传沙箱 /sandbox/project，ADR-187）
	Assignment   string // 分析任务指令（模式A/模式D差异化）
	Timeout      time.Duration
}

// Finding — 沙箱内 DSH 产出的发现（映射前），字段与任务输出契约一致。
type Finding struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Severity    string  `json:"severity"`
	CweID       string  `json:"cwe_id"`
	FilePath    string  `json:"file_path"`
	StartLine   int     `json:"start_line"`
	Confidence  float64 `json:"confidence"`
	Reasoning   string  `json:"reasoning"`
	// ADR-183: 修复建议两通道——fix_suggestion 人类可读 markdown；diff_patch 为
	// apply_patch 语法补丁文本（上下文/删除行须与项目源码逐字一致，服务端校验重建）。
	FixSuggestion string `json:"fix_suggestion"`
	DiffPatch     string `json:"diff_patch"`
}

// Result — 执行结果（形状与旧 runner 一致，调用方零改动）。
type Result struct {
	OK       bool      `json:"ok"`
	Findings []Finding `json:"findings"`
	Error    string    `json:"error"`
	// FinalText — 最终 assistant 消息全文（ADR-183 补遗②：失败反馈再生成回合的
	// 输出契约是 {"patches":[...]} 而非 findings，调用方需原文自行解析）。
	FinalText string `json:"final_text"`
	// ToolCalls — 回合内捕获的工具调用（ADR-184：submit_findings/submit_patches
	// 参数=模型原生 function-calling JSON，优先于文本围栏解析）。
	ToolCalls []ToolCall `json:"tool_calls"`
	Meta      struct {
		SubagentID string `json:"subagent_id"` // 沙箱名
		ExitCode   int    `json:"exit_code"`   // 0=分析回合正常收束；1=异常
		StdoutTail string `json:"stdout_tail"` // 最终 assistant 消息尾部（调试用）
		StderrTail string `json:"stderr_tail"` // 失败时沙箱内 bridge.log 尾部
	} `json:"meta"`
}

// Config — 全局配置 dsh_runtime.sandbox 段（ADR-137/ADR-166/ADR-168）。
type Config struct {
	Mode              string // "off" | "openshell"
	ManagerURL        string // openshell-manager 地址（env OPENSHELL_MANAGER_URL 优先）
	ManagerToken      string // Bearer token（env OPENSHELL_MANAGER_TOKEN 优先；空=共享事实源/免鉴权环回）
	ManagerConfig     string // 共享 config.json 路径（空=兄弟布局 ../openshell-manager/config.json）
	Workspace         string // 网关工作区（沙箱归属）
	Image             string // DSH 沙箱镜像（dsh-pentest-sse，事实源 CD/dsh-pentest-sse/）
	WaitReadyTimeoutS int    // wait-ready 上限（07 §8）
	ExecTimeoutS      int    // 单次沙箱执行上限（07 §8 OpenShell 沙箱执行 30m 的本地映射）
	DSHMaxTokens      int    // bridge DSH_MAX_TOKENS（provider 上限覆盖，ADR-166 补遗同源）
	// GatewayDialAddr — 网关服务路由拨号地址（host:port）。沙箱服务域
	// {workspace}--{sandbox}--{service}.openshell.internal 无 DNS 通配（实测解析到
	// 无关地址），须对网关 IP 直拨并以 Host 头路由；空=由 manager URL host 推导 :8080。
	GatewayDialAddr string
	// OnHumanLog — AI 交互日志出口（人性化流）：按 event type 渲染的中文行与流式正文，
	// 实时、尽力而为、nil 安全。这是 GetAIInteractionLog 面向用户的 chunk 内容。
	OnHumanLog func(line string)
	// OnRawLog — 原始 SSE 帧出口（逐行含换行）：磁盘留存供机器调试（.sse.log），
	// 不经 RPC 面向用户。nil 安全。
	OnRawLog func(chunk []byte)
	// EventFn — 可选生命周期事件出口（level: "info"|"warn"|"error"）；GUI 执行日志通道
	//（ADR-167）。传输层保持 proto 无关，级别用字符串由调用方映射。
	EventFn func(level, msg string)
}

// event — 生命周期事件上报（nil 安全；格式化同 log.Printf 风格）。
func (r *ManagerRunner) event(level, format string, args ...any) {
	if r.cfg.EventFn != nil {
		r.cfg.EventFn(level, fmt.Sprintf(format, args...))
	}
}

// humanLog — 人性化流出口（nil 安全）。
func (r *ManagerRunner) humanLog(s string) {
	if r.cfg.OnHumanLog != nil && s != "" {
		r.cfg.OnHumanLog(s)
	}
}

// rawLog — 原始帧出口（nil 安全）。
func (r *ManagerRunner) rawLog(p []byte) {
	if r.cfg.OnRawLog != nil && len(p) > 0 {
		r.cfg.OnRawLog(p)
	}
}

// manager / 沙箱内路径常量（ADR-166/ADR-168）。
const (
	defaultManagerURL = "http://127.0.0.1:18800"
	serviceRoutePort  = "8080"                        // 网关服务路由端口（CD/openshell-gateway 口径）
	bridgeCmd         = "node /opt/bridge/bridge.mjs" // 镜像内 bridge（CD/dsh-pentest-sse/Dockerfile）
	bridgeHealthz     = "127.0.0.1:8080/healthz"
	bridgeLogPath     = "/sandbox/bridge.log"
	bridgeServiceName = "bridge"
	bridgeTargetPort  = 8080
)

// ManagerRunner — 经 openshell-manager HTTP API 驱动沙箱内 DSH bridge（唯一南向传输层）。
type ManagerRunner struct {
	cfg Config
	hc  *http.Client
}

func NewManagerRunner(cfg Config) *ManagerRunner {
	return &ManagerRunner{cfg: cfg, hc: &http.Client{}}
}

// Enabled — mode=openshell 即启用（manager 不可达在 Run 时 fail-loud 并降级）。
func (r *ManagerRunner) Enabled() bool { return r.cfg.Mode == "openshell" }

// managerEndpoint — 实际生效的 manager 地址：env > 配置 > 共享 config.json > 默认。
// 与引擎 openshell_manager_client.py 同一解析序，两侧不可漂移（ADR-166）。
func (r *ManagerRunner) managerEndpoint() (url, token string) {
	url = firstNonEmpty(os.Getenv("OPENSHELL_MANAGER_URL"), r.cfg.ManagerURL,
		sharedConfig(r.cfg.ManagerConfig).url, defaultManagerURL)
	token = firstNonEmpty(os.Getenv("OPENSHELL_MANAGER_TOKEN"), r.cfg.ManagerToken,
		sharedConfig(r.cfg.ManagerConfig).token)
	return strings.TrimRight(url, "/"), token
}

type sharedCfg struct{ url, token string }

// sharedConfigCandidates — 共享 config.json 的候选路径（按序取第一个可读的）：
// 显式指定 > cwd 相对（部署约定 cwd=仓库根）> 可执行文件相对（工具链布局
// .toolchain/bin/<svc> → 仓库根 → 兄弟目录；与进程 cwd 无关——ADR-167 补遗：
// 服务从任意目录启动都不得丢 token）。
func sharedConfigCandidates(explicit string) []string {
	var out []string
	if explicit != "" {
		out = append(out, explicit)
	}
	if env := os.Getenv("OPENSHELL_MANAGER_CONFIG"); env != "" {
		out = append(out, env)
	}
	out = append(out, filepath.Join("..", "openshell-manager", "config.json"))
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe),
			"..", "..", "..", "..", "openshell-manager", "config.json"))
	}
	return out
}

// sharedConfig — 读 openshell-manager 全局 config.json（两侧共用的唯一事实源），
// token 支持 tokenFile（相对 config.json 所在目录）。全部候选不可读=空（env/配置兜底）。
func sharedConfig(explicit string) sharedCfg {
	for _, path := range sharedConfigCandidates(explicit) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		cfg := sharedCfg{}
		if v, ok := m["url"].(string); ok {
			cfg.url = strings.TrimSpace(v)
		}
		if v, ok := m["token"].(string); ok {
			cfg.token = strings.TrimSpace(v)
		}
		if cfg.token == "" {
			if tf, ok := m["tokenFile"].(string); ok && strings.TrimSpace(tf) != "" {
				fp := tf
				if !filepath.IsAbs(fp) {
					fp = filepath.Join(filepath.Dir(path), tf)
				}
				if b, err := os.ReadFile(fp); err == nil {
					cfg.token = strings.TrimSpace(string(b))
				}
			}
		}
		return cfg
	}
	return sharedCfg{}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// manager HTTP 传输（JSON in/out；错误一律 fail-loud，无直连回退）
// ---------------------------------------------------------------------------

func (r *ManagerRunner) call(ctx context.Context, method, path, token string, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openshell-manager unreachable (%s %s): %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("openshell-manager read body: %w", err)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("openshell-manager %s %s -> HTTP %d: non-JSON body", method, path, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := out["error"].(string)
		return out, fmt.Errorf("openshell-manager %s %s -> HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return out, nil
}

// sandboxRef — manager 返回的沙箱状态投影（gateway.py _ref 的 Go 侧取子集）。
type sandboxRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Phase     int    `json:"phase"`
	PhaseName string `json:"phase_name"`
}

// Run — 完整生命周期一次调用（ADR-168 bridge 通道，ADR-187 项目上传式）：
// launch → 整项目 tar 上传 /sandbox/project → 单轮路径告知 prompt →
// 收敛提取 findings → 恒 teardown。与 RunSession（ADR-173 多轮会话）共享
// launch/uploadProject/turn 通道。代码全文不进 prompt（旧 _inline_workspace
// 链路已废除：大型项目受 prompt 字节上限约束，根本不可行——人类指令 2026-09-03）。
func (r *ManagerRunner) Run(ctx context.Context, t Task) (*Result, error) {
	if !r.Enabled() {
		return nil, ErrDisabled
	}
	if t.WorkspaceDir == "" {
		return nil, fmt.Errorf("workspace dir is empty")
	}

	ls, err := r.launch(ctx, t.TaskID)
	if err != nil {
		return nil, err
	}
	defer ls.teardown()

	if err := r.uploadProject(ctx, ls, t.WorkspaceDir); err != nil {
		return nil, fmt.Errorf("upload project: %w", err)
	}

	res := &Result{}
	res.Meta.SubagentID = ls.ref.Name

	prompt := buildTurnPrompt(t)
	r.event("info", "提交审计任务（prompt %d 字节）", len(prompt))
	finalText, calls, err := ls.turn(ctx, prompt)
	if err != nil {
		r.event("error", "回合失败: %v", err)
		res.Error = err.Error()
		res.Meta.StderrTail = r.bridgeLogTail(ctx, ls.url, ls.token, ls.ref.ID)
		return res, err
	}

	r.event("info", "DSH 回合收敛（最终消息 %d 字节）", len(finalText))
	res.Meta.ExitCode = 0
	res.Meta.StdoutTail = tail(finalText, 400)
	res.FinalText = finalText
	res.ToolCalls = calls

	// ADR-184：结果源优先级——submit_findings 工具参数（原生 function-calling，
	// Cline 同款结构层）> 最终消息 ```json 围栏（降级通道，两代兼容）。
	findings, srcCh, perr := parseAuditResult(finalText, calls)
	if perr != nil {
		r.event("error", "DSH 结果解析失败: %v", perr)
		res.Error = perr.Error()
		return res, fmt.Errorf("result parse failed: %w", perr)
	}
	r.event("info", "DSH 结果解析成功 findings=%d（通道=%s）", len(findings), srcCh)
	res.OK = true
	res.Findings = findings
	return res, nil
}

// SubmitFindingsTool / SubmitPatchesTool — 结构化提交工具名
// （plugins/codeaudit-submit 注册，ADR-184）。
const (
	SubmitFindingsTool = "submit_findings"
	SubmitPatchesTool  = "submit_patches"
)

// LastToolCallArgs — 取指定工具最后一次调用的参数原文（无则 ok=false）。
func LastToolCallArgs(calls []ToolCall, name string) (string, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == name {
			return calls[i].Arguments, true
		}
	}
	return "", false
}

// parseAuditResult — 结果源选择：submit_findings 工具参数优先（模型 function-calling
// 原生产出，无散文转义负担），```json 围栏降级（工具未调用/参数损坏时兜底）。
// ADR-194：分批提交合并——发现较多时模型被要求每批 ≤4 条连续多次调用
// submit_findings（单批巨型参数=数万 token 长流，实测连续断流），此处合并全部
// 批次并按 title+file+line 去重（模型自纠重试可能重发同批）。
// 空列表陷阱（gw-5a96f1f7 实证修复）：submit_findings 提交 {"findings":[]} 是合法
// 产出（模型完整审计后判定无漏洞）——判定依据须是"至少一批参数解析成功"，而不是
// len(merged)>0；后者把干净零发现误判为"两代通道皆空"→ "no JSON in DSH output"
// 整轮报废，上游降级 RuleScan。
// 返回 (findings, 通道名, err)。
func parseAuditResult(finalText string, calls []ToolCall) ([]Finding, string, error) {
	var merged []Finding
	anyParsed := false
	seen := map[string]bool{}
	for i := range calls {
		if calls[i].Name != SubmitFindingsTool {
			continue
		}
		fs, err := ParseFindings(calls[i].Arguments)
		if err != nil {
			continue // 单批损坏不丢弃其余批次（与围栏降级同宽容度）
		}
		anyParsed = true
		for _, f := range fs {
			key := fmt.Sprintf("%s|%s|%d", f.Title, f.FilePath, f.StartLine)
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, f)
		}
	}
	if anyParsed {
		return merged, SubmitFindingsTool, nil // merged 可为空：零发现≠未提交
	}
	// 工具未被调用（或全部批次损坏）→ 围栏降级（不因首选通道失败而丢弃回合）
	fs, err := ParseFindings(finalText)
	if err != nil {
		return nil, "", err
	}
	return fs, "text-fence", nil
}

// execIn — 沙箱内同步执行（manager /api/v1/sandboxes/exec；sandbox_id=创建响应 UUID）。
func (r *ManagerRunner) execIn(ctx context.Context, url, token, sandboxID, script string, timeoutS int) (map[string]any, error) {
	res, err := r.call(ctx, "POST", url+"/api/v1/sandboxes/exec", token, map[string]any{
		"sandbox_id":      sandboxID,
		"command":         []string{"/bin/bash", "-c", script},
		"timeout_seconds": timeoutS,
	})
	if err != nil {
		return nil, err
	}
	if code, _ := res["exit_code"].(float64); int(code) != 0 {
		stderr, _ := res["stderr"].(string)
		stdout, _ := res["stdout"].(string)
		return res, fmt.Errorf("in-sandbox exit %d: %s%s", int(code), tail(stdout, 200), tail(stderr, 200))
	}
	return res, nil
}

// bridgeLogTail — 失败诊断：取沙箱内 bridge 日志尾部（尽力而为，失败返回空）。
func (r *ManagerRunner) bridgeLogTail(ctx context.Context, url, token, sandboxID string) string {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := r.execIn(dctx, url, token, sandboxID, "tail -c 800 "+bridgeLogPath, 20)
	if err != nil {
		return ""
	}
	out, _ := res["stdout"].(string)
	return tail(out, 800)
}

// dialAddr — 网关服务路由拨号地址：显式配置 > manager URL host 推导 :8080。
func (r *ManagerRunner) dialAddr(managerURL string) string {
	if r.cfg.GatewayDialAddr != "" {
		return r.cfg.GatewayDialAddr
	}
	if u, err := url.Parse(managerURL); err == nil {
		if host := u.Hostname(); host != "" {
			return net.JoinHostPort(host, serviceRoutePort)
		}
	}
	return net.JoinHostPort("127.0.0.1", serviceRoutePort)
}

// ---------------------------------------------------------------------------
// bridge HTTP+SSE 会话（ADR-168；wire 契约见 CD/dsh-pentest-sse/bridge.mjs）
// ---------------------------------------------------------------------------

// ToolCall — 回合内捕获的一次模型工具调用（ADR-184 结构化提交通道）。
// arguments 为模型 function-calling 原样 JSON 串（与 Cline ToolCallBlock.arguments 同口径）。
type ToolCall struct {
	Name      string
	Arguments string
}

// bridgeEvent — 流回显中与本执行器收敛判定相关的事件投影。
type bridgeEvent struct {
	turnStart     bool      // turn/start（多轮会话中界定回合边界，ADR-173）
	idle          bool      // 主会话 session.status → idle（回合终态；ADR-190 只认 main）
	assistantText string    // 最近一条 assistant/message 文本
	toolCall      *ToolCall // tool/call（submit_findings/submit_patches 等结构化提交）
	err           error     // 回合级错误（主会话 turn/end reason=error / runtime.exit）
	// agentErr — 子任务回合错误（推理流中断等，ADR-190）：非空=该子任务已死，
	// 其回报永不到达。不置 err（子任务失败 ≠ 整体失败——主会话可基于已有
	// 信息继续），由 turn() 决定是否驱动恢复。
	agentErr string
}

type bridgeSession struct {
	svcURL string
	dial   string // 网关拨号地址（沙箱服务域无 DNS 通配，直拨网关 + Host 头路由）
	hc     *http.Client

	// ADR-200 归因: 本连接累计 SSE 行数与最近一行时间（原子量，stream goroutine 写、
	// turn 错误路径读）——断流时区分"沙箱→dsh-runtime 这一跳断"（行数停滞）与
	// "DSH→上游断"（行流正常但收到 turn/end error 帧）。
	lines atomic.Int64
	last  atomic.Int64
}

// HopStats — 连接级归因快照。
func (b *bridgeSession) HopStats() (lines int64, lastFrameAge time.Duration) {
	last := b.last.Load()
	if last == 0 {
		return b.lines.Load(), -1
	}
	return b.lines.Load(), time.Since(time.Unix(0, last))
}

// routeReq — 以网关直拨 + Host 头构造服务路由请求（等价 curl --resolve）。
func (b *bridgeSession) routeReq(ctx context.Context, method, rawPath string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(b.svcURL)
	if err != nil {
		return nil, fmt.Errorf("service url: %w", err)
	}
	host := u.Host
	req, err := http.NewRequestWithContext(ctx, method, "http://"+b.dial+rawPath, body)
	if err != nil {
		return nil, err
	}
	req.Host = host // 网关按 Host 头路由到 {workspace}--{sandbox}--{service}
	return req, nil
}

// prompt — POST /prompt 提交任务文本（sessionId 固定 main：一次沙箱一个审计会话）。
func (b *bridgeSession) prompt(ctx context.Context, text string) error {
	payload, err := json.Marshal(map[string]any{"sessionId": "main", "text": text})
	if err != nil {
		return err
	}
	req, err := b.routeReq(ctx, "POST", "/prompt", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge /prompt -> HTTP %d: %s", resp.StatusCode, tail(string(raw), 200))
	}
	return nil
}

// stream — 订阅 /events SSE：原始字节行交 onRaw（磁盘留存），结构化事件交 events，
// 人性化渲染行交 onHuman（面向用户）。沙箱内 DSH 会话事件可内联完整任务文本
// （数 MB 级），读缓冲按 12MB 上界放开。
func (b *bridgeSession) stream(ctx context.Context, onRaw func([]byte), onHuman func(string), events chan<- bridgeEvent) error {
	req, err := b.routeReq(ctx, "GET", "/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sse connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse connect -> HTTP %d", resp.StatusCode)
	}
	p := &sseParser{onHuman: onHuman} // 每流独立解析状态（并发任务互不串扰）
	rdr := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := rdr.ReadBytes('\n')
		if len(line) > 0 {
			b.lines.Add(1)
			b.last.Store(time.Now().UnixNano())
			onRaw(line) // 原始帧逐行外流（含换行；含 event:/data:/心跳注释行）
			if ev := p.line(line); ev != nil {
				events <- *ev
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil // 主动取消（收敛后 teardown）不算错误
			}
			return fmt.Errorf("sse read: %w", err)
		}
	}
}

// sseParser — 行式 SSE 解析器（bridge 帧 = "event: <名>\ndata: <JSON>\n\n"，
// 心跳 ": ping" 注释行忽略）。状态属流所有：stream 每连接新建，不跨流共享。
//
// 会话分流（ADR-181）：bridge 把沙箱内 DSH 主会话与其派生的子智能体（Task 工具）
// 事件复用到同一 SSE 流，每帧顶层带 sessionId（main=主会话，UUID=子任务）。子任务
// 的流式增量与主会话逐字交错曾致日志乱码（gw-3b0b9ebf 实证）——因此只有主会话
// 流式渲染思考/输出正文；子任务仅渲染"启动/任务全文/回报"骨架，正文不倾倒。
// 收敛投影（idle/assistantText/turn 错误）同样只认主会话：子任务回合错误不得
// 误伤整体收敛判定。
//
// 面向用户的噪音过滤（ADR-181，人类反馈 2026-09-02）：模型路由/审批策略/系统
// 上下文注入/用量/tool-call 块不再输出——人类视角只需回合、任务全文、思考与输出。
type sseParser struct {
	pending string              // 当前事件块的事件名（event: 行 → data: 行）
	mode    string              // 主会话当前流式块类型（reasoning/text/tool），控制输出头
	agents  map[string]*agentSt // 子任务会话状态（去重骨架行）
	onHuman func(string)        // 人性化行出口（nil 安全）
}

// agentSt — 子任务会话的骨架行去重与生命周期状态。
type agentSt struct {
	announced bool // 已输出"启动"行
	taskShown bool // 已输出任务全文（inbox 与 user/message 双通道只取一次）
	done      bool // turn/end 非 error（正常收束）
	errored   bool // turn/end reason=error（推理流中断等——回报永不到达，ADR-190）
}

const mainSessionID = "main" // bridge 主会话（prompt 固定 sessionId=main）

// agentShort — 子任务会话短标识（UUID 前 8 位，时间线可读）。
func agentShort(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

func (p *sseParser) line(line []byte) *bridgeEvent {
	s := strings.TrimRight(string(line), "\r\n")
	switch {
	case strings.HasPrefix(s, "event: "):
		p.pending = strings.TrimPrefix(s, "event: ")
		return nil
	case strings.HasPrefix(s, "data: "):
		return p.consume(p.pending, s[6:])
	case s == "":
		p.pending = ""
		return nil
	}
	return nil
}

// emit — 流式正文输出（模型原话，原样不加工；nil 安全）。
func (p *sseParser) emit(s string) {
	if p.onHuman != nil && s != "" {
		p.onHuman(s)
	}
}

// emitLine — 元信息行输出（自带换行，与前后内容分行）。
func (p *sseParser) emitLine(s string) {
	if p.onHuman != nil && s != "" {
		p.onHuman(s + "\n")
	}
}

func (p *sseParser) consume(kind, data string) *bridgeEvent {
	var payload any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return nil
	}
	m, _ := payload.(map[string]any)
	switch kind {
	case "bridge.hello":
		p.emitLine("══ DSH 会话开始（bridge）══")
	case "session.status":
		if m != nil {
			sid, _ := m["sessionId"].(string)
			switch st, _ := m["status"].(string); st {
			case "running":
				// 会话运行中=过程噪音，静默（ADR-181）
			case "idle":
				// ADR-190：idle 收敛只认主会话——子任务（后台子代理）会话的 idle
				// 不是回合终态（gw-391b10f1 实证：子代理 idle 曾被误判收敛 → 主会话
				// 尚未产出 submit_findings 即拆沙箱 → "no JSON in DSH output"）。
				if sid != "" && sid != mainSessionID {
					p.emitLine(fmt.Sprintf("🤖 [子任务 %s] 空闲", agentShort(sid)))
					return nil
				}
				p.emitLine("■ 会话空闲（收束）")
				return &bridgeEvent{idle: true}
			}
		}
	case "runtime.exit":
		p.emitLine(fmt.Sprintf("⚠ DSH 运行时退出: %s", data))
		return &bridgeEvent{err: fmt.Errorf("bridge runtime exited: %s", data)}
	case "session.event":
		ev, _ := m["event"].(map[string]any)
		if ev == nil {
			return nil
		}
		sid, _ := m["sessionId"].(string)
		if sid != "" && sid != mainSessionID {
			return p.agentEvent(sid, ev) // 子任务事件：骨架渲染 + 死亡信号（ADR-190）
		}
		return p.sessionEvent(ev)
	}
	return nil
}

// agentEvent — 子任务会话骨架渲染：启动行 + 任务全文一次；流式正文静默。
// turn/end reason=error（推理流中断等）时返回 agentErr 死亡信号（ADR-190）——
// 主会话在等该子任务回报，此信号供 turn() 决定恢复驱动。
func (p *sseParser) agentEvent(sid string, ev map[string]any) *bridgeEvent {
	st := p.agents[sid]
	if st == nil {
		if p.agents == nil {
			p.agents = map[string]*agentSt{}
		}
		st = &agentSt{}
		p.agents[sid] = st
	}
	if !st.announced {
		st.announced = true
		p.emitLine(fmt.Sprintf("🤖 [子任务 %s] 启动", agentShort(sid)))
	}
	et, _ := ev["type"].(string)
	if et == "turn/end" {
		data, _ := ev["data"].(map[string]any)
		reason, _ := data["reason"].(map[string]any)
		kind, _ := reason["kind"].(string)
		if kind == "error" {
			eo, _ := reason["error"].(map[string]any)
			msg, _ := eo["message"].(string)
			st.errored = true
			p.emitLine(fmt.Sprintf("🤖 [子任务 %s] ❌ 推理流中断（%s）", agentShort(sid), msg))
			return &bridgeEvent{agentErr: fmt.Sprintf("%s: %s", agentShort(sid), msg)}
		}
		st.done = true
		p.emitLine(fmt.Sprintf("🤖 [子任务 %s] 回合结束", agentShort(sid)))
		return nil
	}
	if et != "agent/inbox/spliced" && et != "user/message" || st.taskShown {
		return nil
	}
	data, _ := ev["data"].(map[string]any)
	if data == nil {
		return nil
	}
	text := ""
	if et == "user/message" {
		text = userText(data)
	} else {
		text = splicedText(data)
	}
	if text == "" || isSystemContext(text) {
		return nil
	}
	st.taskShown = true
	p.emitLine(fmt.Sprintf("🤖 [子任务 %s] 任务（%d 字节）", agentShort(sid), len(text)))
	p.emitLine(text)
	return nil
}

// sessionEvent — 主会话事件按 type 人性化并做收敛投影。
func (p *sseParser) sessionEvent(ev map[string]any) *bridgeEvent {
	et, _ := ev["type"].(string)
	data, _ := ev["data"].(map[string]any)
	switch et {
	case "permission/preset", "sandbox/mode", "approval/policy", "approval/asked", "approval/decided",
		"session/title", "request/header", "request/context", "todo/write":
		// 模型路由/审批策略/系统注入/请求元信息=机器噪音，静默（ADR-181 人类反馈）
	case "turn/start":
		p.emitLine(fmt.Sprintf("── 第 %v 轮开始 ──", data["turn"]))
		return &bridgeEvent{turnStart: true}
	case "step/start", "step/end", "agent/inbox/spliced":
		// 步骤粒度/收件箱注入对人不增值：轮次头已足够，静默
	case "user/message":
		p.userMessage(data)
	case "assistant/chunk":
		chunk, _ := data["chunk"].(map[string]any)
		return p.assistantChunk(chunk)
	case "assistant/message":
		// 最终消息已由 text-delta 流式呈现，不重复输出；仅作收敛投影
		return &bridgeEvent{assistantText: messageText(data)}
	case "tool/call":
		// ADR-184：结构化提交通道——submit_findings/submit_patches 的参数经模型
		// 原生 function-calling 产出（不再让模型在散文里手写 JSON 围栏）。
		name, _ := data["name"].(string)
		args, _ := data["arguments"].(string)
		p.emitLine(fmt.Sprintf("⟐ 工具调用 %s（参数 %d 字节）", name, len(args)))
		return &bridgeEvent{toolCall: &ToolCall{Name: name, Arguments: args}}
	case "turn/end":
		reason, _ := data["reason"].(map[string]any)
		kind, _ := reason["kind"].(string)
		if kind == "error" {
			eo, _ := reason["error"].(map[string]any)
			msg, _ := eo["message"].(string)
			p.emitLine(fmt.Sprintf("── 回合结束: ❌ 错误（%s）──", msg))
			return &bridgeEvent{err: fmt.Errorf("turn error: %s", msg)}
		}
		p.emitLine(fmt.Sprintf("── 回合结束: %s ──", kind))
	default:
		// 未识别类型静默——保持可读性（原始帧在磁盘 .sse.log 可查）
	}
	return nil
}

// isSystemContext — 系统上下文注入判定（运行时上下文/系统提醒，不出正文）。
func isSystemContext(text string) bool {
	return strings.HasPrefix(text, "Current runtime context") ||
		strings.HasPrefix(text, "<system-reminder>")
}

// userText — user/message 事件的文本块聚合。
func userText(data map[string]any) string {
	blocks, _ := data["content"].([]any)
	text := ""
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		if bm["type"] == "text" {
			if t, ok := bm["text"].(string); ok {
				text += t
			}
		}
	}
	return text
}

// splicedText — agent/inbox/spliced 事件 inserted 文本块聚合（子任务任务文本通道）。
func splicedText(data map[string]any) string {
	text := ""
	inserted, _ := data["inserted"].([]any)
	for _, item := range inserted {
		im, _ := item.(map[string]any)
		blocks, _ := im["content"].([]any)
		for _, blk := range blocks {
			bm, _ := blk.(map[string]any)
			if bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok {
					text += t
				}
			}
		}
	}
	return text
}

// userMessage — 主会话任务下发：完整正文输出（ADR-181 人类反馈"完整输出下发的
// 提示词"）；系统上下文静默；子任务回报单独标记（正文完整保留）。
func (p *sseParser) userMessage(data map[string]any) {
	text := userText(data)
	if text == "" {
		return
	}
	if isSystemContext(text) {
		return // 系统上下文已注入——静默（ADR-181 人类反馈）
	}
	if strings.HasPrefix(text, "Background subagent") && strings.Contains(text, "reported:") {
		p.emitLine(fmt.Sprintf("📋 [子任务回报]（%d 字节）", len(text)))
		p.emitLine(text)
		return
	}
	p.emitLine(fmt.Sprintf("📋 [任务下发]（%d 字节）", len(text)))
	p.emitLine(text)
}

// assistantChunk — 主会话流式 chunk：思考/输出正文原样流出（模型原话）；
// 工具调用与用量静默（ADR-181 人类反馈——tool-call 过程与 tokens 对人无读值）。
func (p *sseParser) assistantChunk(chunk map[string]any) *bridgeEvent {
	ct, _ := chunk["type"].(string)
	switch ct {
	case "block-start":
		bt, _ := chunk["blockType"].(string)
		p.mode = bt
		switch bt {
		case "reasoning":
			p.emit("\n💭 [思考]\n")
		case "text":
			p.emit("\n✍ [输出]\n")
		default:
			// tool_use / tool-call 等机器块：静默（block-end 同理不补摘要）
		}
	case "block-end":
		if p.mode == "reasoning" || p.mode == "text" {
			p.emit("\n")
		}
		p.mode = ""
	case "reasoning-delta":
		if p.mode != "reasoning" {
			p.mode = "reasoning"
			p.emit("\n💭 [思考]\n")
		}
		if t, _ := chunk["text"].(string); t != "" {
			p.emit(t)
		}
	case "text-delta":
		if p.mode != "text" {
			p.mode = "text"
			p.emit("\n✍ [输出]\n")
		}
		if t, _ := chunk["text"].(string); t != "" {
			p.emit(t)
		}
	case "tool-call-delta", "usage", "finish":
		// 工具参数为机器 JSON 片段；用量为记账信息——均静默（ADR-181）
	}
	return nil
}

// messageText — assistant/message 的纯文本聚合（收敛投影用）。
func messageText(data map[string]any) string {
	msg, _ := data["message"].(map[string]any)
	blocks, _ := msg["content"].([]any)
	var b strings.Builder
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		if bm["type"] == "text" {
			if t, ok := bm["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// sandboxSpec — openshell.v1.SandboxSpec dict。
// 依据: 引擎 openshell_runtime_real.py build_sandbox_spec + sandbox_policy_dict
// （policy.version=1 为网关不变量；landlock best_effort=docker driver 降级语义）。
func sandboxSpec(name, taskID, image string) map[string]any {
	return map[string]any{
		"log_level": "info",
		"environment": map[string]string{
			"DSH_TASK_ID": taskID,
		},
		"template": map[string]any{
			"image": image,
			"labels": map[string]string{
				"openshell.io/managed-by":   "codeaudit-dsh-runtime",
				"openshell.io/sandbox-name": name,
			},
		},
		"policy": map[string]any{
			"version": 1, // 静态创建时策略版本必须为 1（网关不变量）
			"filesystem": map[string]any{
				"include_workdir": true,
				"read_only":       []string{"/skills", "/tools", "/input"},
				"read_write":      []string{"/sandbox", "/tmp"},
			},
			"landlock": map[string]any{"compatibility": "best_effort"},
			"process": map[string]any{
				"run_as_user": "sandbox", "run_as_group": "sandbox",
			},
		},
		"providers": []string{},
	}
}

// randomHex12 — 12 位十六进制随机串（crypto/rand），用于沙箱名后缀。
func randomHex12() string {
	b := make([]byte, 6)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

func sandboxRefFrom(m map[string]any) sandboxRef {
	var ref sandboxRef
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &ref)
	return ref
}

// ---------------------------------------------------------------------------
// prompt 组装（ADR-187 路径告知式：项目整包上传沙箱 /sandbox/project，DSH 自行
// 读盘分析；代码全文不进 prompt。旧 runner _inline_workspace 内联链路已废除——
// 人类指令 2026-09-03：大型项目内联不可行，上传后告知路径才是正解）
// ---------------------------------------------------------------------------

var walkExcludes = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true, ".toolchain": true,
	".venv": true, "dist": true, "target": true, "vendor": true,
}

// assignmentTemplate — 审计任务指令骨架。占位符：项目沙箱路径、任务指令。
const assignmentTemplate = `# CodeAudit 代码安全分析任务

## 代码范围
待审计项目的完整源码已上传在你所在环境的 %s 目录。请自行阅读该目录中的项目
源码（文件相对路径即其在项目内的路径），结合整个项目追踪数据流与调用关系后分析。

## 任务
%s

## 修复建议要求（ADR-183）
对每个确认的漏洞，除判定结论外还须给出两条修复产出：
1. fix_suggestion：人类可读的修复说明（markdown，简述根因与修法）；
2. diff_patch：机器可应用的补丁，使用 apply_patch 语法，格式规范：
   - 整体结构（一次修复涉及多文件时依次列出多个 Update File 段，放同一个 diff_patch）：
     *** Begin Patch
     *** Update File: <仓库相对路径>
     [@@ <原文件中真实存在的一行内容，作为锚定提示>]
     [上下文行（以单个空格开头）]
     -被删除的行
     +新增的行
     [*** End of File    仅当改动涉及文件末尾时使用]
     *** End Patch
   - 行首字符仅三种：空格=上下文、-=删除、+=新增；上下文行与删除行必须从上述目录中的项目源码逐字复制（含缩进），"@@" 后跟改动块上方最近的一行原文件内容作锚点；
   - 不使用行号（消费端按内容锚定）；同一文件多个改动块按从上到下顺序排列；
   - 支持 *** Add File: / *** Delete File:（新文件内容每行以 + 开头）；不要使用 *** Move to:；
   - 所有文本 NFC 归一，引号用 ASCII 直引号，不要弯引号/智能引号/不间断空格。

## 输出契约（必须遵守）
分析完成后，调用 submit_findings 工具提交全部发现（字段与该工具参数 schema 一致：
title/description/severity/cwe_id/file_path/start_line/confidence/reasoning/
fix_suggestion/diff_patch）。正文只写简短结论摘要，不要在正文里另写 JSON。
**发现较多时分批提交（ADR-194）**：每批最多 4 条——单次提交生成过长的参数流
（12 条+补丁需数万 token）会显著增加推理流中断风险（gw-7f06fe5d 实证：单批
巨型提交连续 4 次断流）。分批时逐批连续调用 submit_findings（服务端自动合并
去重），全部批次提交完成后再写最终摘要。
没有发现时调用 submit_findings 提交 {"findings": []}。
仅当工具不可用时才降级：把最终结论作为一个 JSON 代码块（` + "```json ... ```" + `）
输出为最后一条消息，schema:
{
  "findings": [
    {
      "title": "简短漏洞标题",
      "description": "问题与影响说明",
      "severity": "SEVERITY_CRITICAL|SEVERITY_HIGH|SEVERITY_MEDIUM|SEVERITY_LOW|SEVERITY_INFO",
      "cwe_id": "CWE-XXX 或空",
      "file_path": "项目内相对路径",
      "start_line": 行号整数,
      "confidence": 0.0到1.0,
      "reasoning": "判定理由（引用代码证据）",
      "fix_suggestion": "修复说明 markdown（无把握可空字符串）",
      "diff_patch": "apply_patch 语法补丁文本（无把握可空字符串，不要编造）"
    }
  ]
}
`

// buildTurnPrompt — 路径告知式任务指令组装（纯函数无 IO；prompt 体量与项目大小无关）。
func buildTurnPrompt(t Task) string {
	return fmt.Sprintf(assignmentTemplate, ProjectSandboxPath, t.Assignment)
}

// ParseFindings — 从 DSH 最终消息提取结果 JSON（fence 容错，对齐父项目 envelope 适配纪律）。
var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

func ParseFindings(stdout string) ([]Finding, error) {
	candidate := stdout
	if m := fenceRe.FindStringSubmatch(stdout); m != nil {
		candidate = m[1]
	} else {
		i, j := strings.Index(stdout, "{"), strings.LastIndex(stdout, "}")
		if !(0 <= i && i < j) {
			return nil, fmt.Errorf("no JSON in DSH output")
		}
		candidate = stdout[i : j+1]
	}
	var payload struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
		// 轻量修复层（Cline repairMalformedToolCall 对齐的最小集）：尾逗号。
		// 截断/未闭合字符串不修复——宁可失败走失败反馈再生成（hasUnterminatedString 同精神）。
		repaired := trailingCommaRe{}.apply(candidate)
		if rerr := json.Unmarshal([]byte(repaired), &payload); rerr != nil {
			return nil, fmt.Errorf("findings parse: %w", err)
		}
	}
	return payload.Findings, nil
}

// trailingCommaRe — 去除 }/] 前的尾逗号（LLM 常见 JSON 小错）。
type trailingCommaRe struct{}

func (trailingCommaRe) apply(s string) string {
	for strings.Contains(s, ",}") || strings.Contains(s, ",]") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, ",}", "}"), ",]", "]")
	}
	return s
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// PatchFix — 失败反馈再生成回合的单条补丁修复（ADR-183 补遗②输出契约）。
type PatchFix struct {
	Index     int    `json:"index"`      // 对应审计回合 findings 数组下标（0 基）
	DiffPatch string `json:"diff_patch"` // apply_patch 语法补丁文本
}

// ParsePatches — 从再生成回合最终消息提取 {"patches":[...]}（fence 容错同 ParseFindings）。
func ParsePatches(stdout string) ([]PatchFix, error) {
	candidate := stdout
	if m := fenceRe.FindStringSubmatch(stdout); m != nil {
		candidate = m[1]
	} else {
		i, j := strings.Index(stdout, "{"), strings.LastIndex(stdout, "}")
		if !(0 <= i && i < j) {
			return nil, fmt.Errorf("no JSON in patch-fix output")
		}
		candidate = stdout[i : j+1]
	}
	var payload struct {
		Patches []PatchFix `json:"patches"`
	}
	if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
		if rerr := json.Unmarshal([]byte(trailingCommaRe{}.apply(candidate)), &payload); rerr != nil {
			return nil, fmt.Errorf("patches parse: %w", err)
		}
	}
	return payload.Patches, nil
}
