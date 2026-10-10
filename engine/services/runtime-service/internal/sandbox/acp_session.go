// acpSession — ACP（acp-stdio）方言适配器 v0。
// 形态：管道式回合驱动——每回合在沙箱内起 agent ACP 子进程（如 `opencode acp`），
// 经进程内 python3 驱动器完成 JSON-RPC 握手（initialize→initialized→session/new→
// session/prompt），回合收束（session/prompt 响应）后进程退出、引擎解析全部帧。
// 依据: 主仓库 docs/designs/platform-architecture-2026-09.md §7（R6 driver 契约：
// 方言面以 ACP 方法面为对齐目标；request_permission 由外层代答，v0 全批）；
// descriptor 注册表 acp-stdio 预留位（knownDialects/knownTransports，切片 1）。
//
// 诚实边界（capability 口径，见 agent_providers.yaml opencode 条目）：
//   - 近实时：驱动器逐帧 flush 落盘
//     acp-out.jsonl（驱动器源码即证），引擎 2s 尾读增量→acpStream 逐帧渲染直喂
//     humanLog——经既有 ADR-189 流式链（StreamAIInteractionLog→网关 WS 250ms 聚合）
//     到达前端。粒度=2s 轮询窗（exec 模型无推送通道）；250ms 级=v1 bridge
//     （沙箱常驻 ACP 进程+SSE，session/acp 模式，架构档既定方向）。
//   - 会话恢复：v0 每回合新会话（resume:none）；acp-session-load=v1。
//   - 图任务：openGraphSession 不注册 acp（图×acp 属后续批，诚实报错面既有）。
//   - 结果：text-final 通道（无 codeaudit-submit 工具），解析走 ADR-184 兜底。
//
// 批式预言机（parseACPExchange/acpExchange/splitACPReadout）生产零调用——它们是
// acpStream 的批式等价预言机，仅测试引用，居 acp_session_test.go（2026-09-22
// 等价简化批自本文件移出）。
package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	acpExecEnv     = "HOME=/home/node"
	acpProjectPath = "/sandbox" // ACP cwd（dsh 桥同口径；uploadProject 落点校准随 provisioning 批）
	// acpTurnBudgetDefaultS — 回合总预算缺省秒。07 §8 EXEC_TIMEOUT_MAX 单次 exec 600s
	// 钳制——两段式后台+轮询绕行（同 maka 复批 II C4 形态）；缺省 1800=30m，
	// env 可调（CODEAUDIT_ACP_TURN_BUDGET_S，非法值回落缺省）。
	acpTurnBudgetDefaultS = 1800
	// acpFiles — 驱动器在沙箱内的固定文件面（/tmp 下，沙箱生命周期=任务生命周期）。
	acpPromptFile = "/tmp/acp-prompt.txt"
	acpCmdFile    = "/tmp/acp-cmd.json"
	acpMetaFile   = "/tmp/acp-meta.json"
	acpDriverFile = "/tmp/acp-drive.py"
	acpOutFile    = "/tmp/acp-out.jsonl"
	acpErrFile    = "/tmp/acp-err.log"
	acpDoneFile   = "/tmp/acp-done"
)

// acpTurnBudgetS — 回合总预算（env 覆盖 > 缺省 30m）。
func acpTurnBudgetS() int {
	if v := os.Getenv("CODEAUDIT_ACP_TURN_BUDGET_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return acpTurnBudgetDefaultS
}

// acpDriverScript — 沙箱内 python3 驱动器：反应式 JSON-RPC 握手 + 服务端请求代答。
// 输出 /tmp/acp-out.jsonl = 原始帧逐行留存 + _driver 注记帧（代答/终态/致命）。
// 权限代答（架构 §7"request_permission 由桥按外层策略代答，v0 全批"）：选项取首项
// selected——管道式无法交互，代答留痕（注记帧+⚠ 行进 humanLog）。
const acpDriverScript = `import json, subprocess, sys, time

def rj(p):
    with open(p, encoding='utf-8') as f: return json.load(f)

cmd = rj('/tmp/acp-cmd.json')
meta = rj('/tmp/acp-meta.json')
prompt = open('/tmp/acp-prompt.txt', encoding='utf-8').read()
cwd = meta.get('cwd', '/sandbox')
budget = int(meta.get('budget_s', 1800))
out = open('/tmp/acp-out.jsonl', 'w', encoding='utf-8')
err = open('/tmp/acp-err.log', 'w', encoding='utf-8')
try:
    p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=err, cwd=cwd)
except Exception as e:
    out.write(json.dumps({'_driver': 'fatal', 'why': 'spawn: %s' % e}) + '\n'); out.flush(); sys.exit(3)

def send(fr):
    p.stdin.write((json.dumps(fr, ensure_ascii=False) + '\n').encode('utf-8')); p.stdin.flush()

def note(kind, **kv):
    out.write(json.dumps({'_driver': kind, **kv}, ensure_ascii=False) + '\n'); out.flush()

send({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize',
      'params': {'protocolVersion': 1, 'clientCapabilities': {},
                 'clientInfo': {'name': 'codeaudit-engine', 'version': '0'}}})
deadline = time.time() + budget
sid, done = None, False
while not done and time.time() < deadline:
    line = p.stdout.readline()
    if not line: break
    try: fr = json.loads(line)
    except Exception: continue
    out.write(line.decode('utf-8', 'replace')); out.flush()
    rid, method = fr.get('id'), fr.get('method')
    if method is not None and rid is not None:
        params = fr.get('params') or {}
        if method == 'session/request_permission':
            opts = params.get('options') or []
            pick = None
            if opts: pick = opts[0].get('optionId') or opts[0].get('id')
            send({'jsonrpc': '2.0', 'id': rid, 'result': {'outcome': 'selected', 'optionId': pick}})
            note('auto_permission', picked=pick)
        else:
            send({'jsonrpc': '2.0', 'id': rid,
                  'error': {'code': -32601, 'message': 'codeaudit acp driver: unsupported server request %s' % method}})
            note('unsupported_request', method=method)
        continue
    if rid is not None and method is None:
        res = fr.get('result') or {}
        if rid == 1:
            send({'jsonrpc': '2.0', 'method': 'initialized'})
            send({'jsonrpc': '2.0', 'id': 2, 'method': 'session/new',
                  'params': {'cwd': cwd, 'mcpServers': []}})
        elif rid == 2:
            sid = res.get('sessionId')
            if not sid:
                note('fatal', why='session/new response missing sessionId'); break
            send({'jsonrpc': '2.0', 'id': 3, 'method': 'session/prompt',
                  'params': {'sessionId': sid, 'prompt': [{'type': 'text', 'text': prompt}]}})
        elif rid == 3:
            note('turn_done', stopReason=res.get('stopReason')); done = True
        continue
if not done:
    note('fatal', why='turn not converged (stream end or budget exhausted)')
try: p.stdin.close()
except Exception: pass
try: rc = p.wait(timeout=30)  # 07 §8 收束等待余量（驱动器尾段，不占回合预算）
except Exception:
    p.kill(); rc = -1
if not done: sys.exit(4)
sys.exit(0)
`

// acpSession — ACP 方言回合会话（turnSession 实现，模板同 maka：launch+上传+健康面）。
type acpSession struct {
	ls      *liveSession
	r       *ManagerRunner
	taskID  string
	tasklog string // .tasklog 根（baton 投影输入导出位；空=禁用——同 maka exportRuntimeEvents 纪律）
	agent   string // descriptor launch.bridge_cmd（acp-stdio 消费语义=agent ACP 子进程命令）
	turns   int
}

// newACPSession — open：launch（镜像经 effectiveImage=descriptor 优先）→ 上传项目 →
// 健康面（cli-help：命令首 token --version）。bridge 不启动（非 sse-bridge 通道）。
func newACPSession(ctx context.Context, r *ManagerRunner, t Task) (turnSession, error) {
	cmd := r.cfg.Agent.Launch.BridgeCmd
	if cmd == "" {
		return nil, fmt.Errorf("acp-stdio 方言要求 descriptor launch.bridge_cmd 显式给出 agent ACP 命令（如 %q）", "opencode acp")
	}
	first := strings.Fields(cmd)[0]
	ls, err := r.launch(ctx, t.TaskID)
	if err != nil {
		return nil, err
	}
	if t.WorkspaceDir != "" {
		if err = r.uploadProject(ctx, ls, t.WorkspaceDir); err != nil {
			ls.teardown()
			return nil, fmt.Errorf("upload project: %w", err)
		}
	}
	if _, err = r.execIn(ctx, ls.url, ls.token, ls.ref.ID,
		fmt.Sprintf("%s %s --version", acpExecEnv, first), 60); err != nil {
		ls.teardown()
		return nil, fmt.Errorf("acp agent healthcheck（%s --version）: %w", first, err)
	}
	r.humanLog(fmt.Sprintf("══ ACP 会话开始（%s · acp-stdio 管道式，近实时 2s 尾读）══\n", first))
	return &acpSession{ls: ls, r: r, taskID: t.TaskID, tasklog: t.TasklogDir, agent: cmd}, nil
}

// turn — 一回合：写输入文件+驱动器 → 两段式后台执行 → 读帧 → 解析渲染。
func (s *acpSession) turn(ctx context.Context, prompt string) (string, []ToolCall, error) {
	s.turns++
	meta := fmt.Sprintf(`{"cwd": %q, "budget_s": %d}`, acpProjectPath, acpTurnBudgetS())
	cmdJSON, _ := json.Marshal(strings.Fields(s.agent))
	writes := []struct{ path, b64 string }{
		{acpPromptFile, base64.StdEncoding.EncodeToString([]byte(prompt))},
		{acpCmdFile, base64.StdEncoding.EncodeToString(cmdJSON)},
		{acpMetaFile, base64.StdEncoding.EncodeToString([]byte(meta))},
		{acpDriverFile, base64.StdEncoding.EncodeToString([]byte(acpDriverScript))},
	}
	for _, w := range writes {
		script := fmt.Sprintf("base64 -d > %s <<'B64EOF'\n%s\nB64EOF", w.path, w.b64)
		if _, err := s.execOnce(ctx, script, 60); err != nil {
			return "", nil, err
		}
	}
	// 两段式（07 §8 单次 exec 600s 钳制绕行，同 maka 复批 II C4）：后台起驱动器，
	// 完成标记轮询（单轮探测 <600s 安全），完成后读输出文件。
	bg := fmt.Sprintf(
		"rm -f %s; ( %s python3 %s > /dev/null 2>&1; echo DONE > %s ) & echo started",
		acpDoneFile, acpExecEnv, acpDriverFile, acpDoneFile)
	if _, err := s.execOnce(ctx, bg, 90); err != nil {
		return "", nil, err
	}
	// v0.5 实时流：回合头即出（prompt 引擎已知——比 dsh 的回显更早），随后 2s
	// 尾读 acp-out.jsonl 增量（驱动器逐帧 flush 落盘——源码即证），完整行喂
	// acpStream 逐帧渲染直出 humanLog（经 ADR-189 流式链到前端 WS）。尾读粒度=
	// 2s 轮询窗（exec 模型无推送通道；250ms 级=v1 bridge）。
	s.r.humanLog(fmt.Sprintf("── 第 %d 轮开始 ──\n📋 [任务下发]（%d 字节）\n", s.turns, len(prompt)))
	s.r.humanLog(prompt + "\n")
	stream := &acpStream{emit: func(t string) { s.r.humanLog(t) }}
	var off int64 // acp-out.jsonl 已消费字节偏移
	drain := func() (string, error) {
		raw, err := s.execOnce(ctx, fmt.Sprintf("tail -c +%d %s 2>/dev/null || true", off+1, acpOutFile), 60)
		if err != nil {
			return "", err
		}
		// 只消费完整行（窗口边界半行留到下轮）
		if n := strings.LastIndexByte(raw, '\n'); n >= 0 {
			off += int64(n + 1)
			return raw[:n+1], nil
		}
		return "", nil
	}
	deadline := time.Now().Add(time.Duration(acpTurnBudgetS()) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		for {
			chunk, err := drain()
			if err != nil || chunk == "" {
				break
			}
			for _, ln := range strings.Split(chunk, "\n") {
				stream.line(ln)
			}
		}
		res, err := s.execOnce(ctx, "cat "+acpDoneFile+" 2>/dev/null || true", 30)
		if err == nil && strings.Contains(res, "DONE") {
			for { // 终局排干
				chunk, err := drain()
				if err != nil || chunk == "" {
					break
				}
				for _, ln := range strings.Split(chunk, "\n") {
					stream.line(ln)
				}
			}
			s.exportACPEvents()
			text := stream.finalText()
			if stream.stop == "" && text == "" && stream.agentName == "" {
				errTail, _ := s.execOnce(ctx, "tail -c 800 "+acpErrFile+" 2>/dev/null || true", 30)
				return "", nil, fmt.Errorf("acp 驱动器未收敛（零帧输出；stderr 尾: %s）", tail(errTail, 300))
			}
			return text, nil, nil
		}
	}
	return "", nil, fmt.Errorf("acp 回合超时（%ds 轮询预算耗尽）", acpTurnBudgetS())
}

// acpStream — ACP 帧的实时渲染器（v0.5：帧到即渲染直喂 humanLog，经既有
// ADR-189 流式链到达前端 WS）。行协议对齐 dsh（✍/▶/⚠/■——parseTimeline 兼容）。
// finalText 累积=assistant 文本块按 messageId 分段（与批式语义一致）。
type acpStream struct {
	emit      func(string)
	agentName string
	sessionID string
	sawOutput bool // ✍ 头只出一次（本回合输出块开头）
	blocks    []string
	curText   strings.Builder
	curMsg    any
	stop      string
}

func (a *acpStream) line(s string) string {
	line := strings.TrimSpace(s)
	if line == "" {
		return ""
	}
	var f map[string]any
	if json.Unmarshal([]byte(line), &f) != nil {
		return "" // 畸形/半行容忍（尾读窗口边界）
	}
	if note, ok := f["_driver"].(string); ok {
		switch note {
		case "turn_done":
			if sr, _ := f["stopReason"].(string); sr != "" {
				a.stop = sr
			}
			a.flushBlock()
			a.emit(fmt.Sprintf("■ 回合收束（stopReason=%s）\n", a.stopOr("end_turn")))
		case "fatal":
			a.flushBlock()
			a.emit(fmt.Sprintf("⚠ ACP 驱动器未收敛: %v\n", f["why"]))
		case "auto_permission":
			a.emit(fmt.Sprintf("⚠ 权限请求已代答（选中 %v）——管道式无交互面\n", f["picked"]))
		case "unsupported_request":
			a.emit(fmt.Sprintf("⚠ 服务端请求未支持已拒答: %v\n", f["method"]))
		}
		return ""
	}
	id, hasID := f["id"].(float64)
	method, _ := f["method"].(string)
	switch {
	case hasID && method == "":
		res, _ := f["result"].(map[string]any)
		switch int(id) {
		case 1:
			if ai, _ := res["agentInfo"].(map[string]any); ai != nil {
				n, _ := ai["name"].(string)
				v, _ := ai["version"].(string)
				a.agentName = strings.TrimSpace(n + " " + v)
				a.emit(fmt.Sprintf("▶ [ACP] agent %s\n", a.agentName))
			}
		case 2:
			a.sessionID, _ = res["sessionId"].(string)
		case 3:
			if sr, _ := res["stopReason"].(string); sr != "" {
				a.stop = sr
			}
			a.flushBlock()
			a.emit(fmt.Sprintf("■ 回合收束（stopReason=%s）\n", a.stopOr("end_turn")))
		}
	case method == "session/update":
		params, _ := f["params"].(map[string]any)
		upd, _ := params["update"].(map[string]any)
		if upd == nil {
			return ""
		}
		tag, _ := upd["sessionUpdate"].(string)
		switch tag {
		case "agent_message_chunk":
			content, _ := upd["content"].(map[string]any)
			if ct, _ := content["type"].(string); ct != "text" {
				a.emit(fmt.Sprintf("▶ [输出块] 非 text 内容（%s）已略\n", ct))
				return ""
			}
			mid := upd["messageId"]
			if mid != a.curMsg && a.curMsg != nil {
				a.flushBlock()
			}
			a.curMsg = mid
			if !a.sawOutput {
				a.sawOutput = true
				a.emit("\n✍ [输出]\n")
			}
			if txt, _ := content["text"].(string); txt != "" {
				a.curText.WriteString(txt)
				a.emit(txt) // 实时：chunk 到即出
			}
		case "user_message_chunk":
			// 我方提示词回显——静默（引擎已在回合头发过任务下发）
		case "tool_call":
			title, _ := upd["title"].(string)
			kind, _ := upd["kind"].(string)
			name, _ := upd["toolCallId"].(string)
			a.emit(fmt.Sprintf("▶ [工具] %s（%s · %s）\n", title, kind, name))
		case "tool_call_update":
			if status, _ := upd["status"].(string); status == "completed" || status == "failed" {
				title, _ := upd["title"].(string)
				a.emit(fmt.Sprintf("▶ [工具·%s] %s\n", status, title))
			}
		case "plan":
			if plan, ok := upd["plan"].([]any); ok {
				a.emit(fmt.Sprintf("▶ [计划] %d 项\n", len(plan)))
			}
		case "available_commands_update", "current_mode_update", "config_option_update":
			// 机器噪音静默（ADR-181 同口径）
		default:
			a.emit(fmt.Sprintf("▶ [未识别 update] %s\n", tag))
		}
	}
	return ""
}

func (a *acpStream) stopOr(def string) string {
	if a.stop != "" {
		return a.stop
	}
	return def
}

// flushBlock — 当前文本块入列（messageId 边界/收束时）。
func (a *acpStream) flushBlock() {
	if a.curText.Len() > 0 {
		a.blocks = append(a.blocks, a.curText.String())
		a.curText.Reset()
	}
	a.curMsg = nil
}

// finalText — 全部 assistant 文本块（\n\n 连接；text-final 解析输入）。
func (a *acpStream) finalText() string {
	a.flushBlock()
	return strings.Join(a.blocks, "\n\n")
}

// execOnce — 单次 manager exec（timeout 钳 580<600 上限，07 §8）。
func (s *acpSession) execOnce(ctx context.Context, script string, timeoutS int) (string, error) {
	if timeoutS > 580 {
		timeoutS = 580 // manager exec 上限 600s（R27 EXEC_TIMEOUT_MAX；07 §8）取 580 余量
	}
	res, err := s.r.call(ctx, "POST", s.ls.url+"/api/v1/sandboxes/exec", s.ls.token, map[string]any{
		"sandbox_id":      s.ls.ref.ID,
		"command":         []string{"/bin/bash", "-c", script},
		"timeout_seconds": timeoutS,
		"env":             map[string]any{"HOME": "/home/node"},
	})
	if err != nil {
		return "", err
	}
	if code, _ := res["exit_code"].(float64); int(code) != 0 {
		stderr, _ := res["stderr"].(string)
		stdout, _ := res["stdout"].(string)
		return "", fmt.Errorf("acp exec exit %d: %s%s", int(code), tail(stdout, 200), tail(stderr, 300))
	}
	out, _ := res["stdout"].(string)
	return out, nil
}

// close — 沙箱回收（teardown 恒回收纪律同 dsh/maka）。幂等（teardown 自带闸）。
func (s *acpSession) close() { s.ls.teardown() }

// name — 会话标识（沙箱 name，同 dsh/maka 口径）。
func (s *acpSession) name() string {
	if s.ls.ref.Name != "" {
		return s.ls.ref.Name
	}
	return s.ls.ref.ID
}

// turnsContribution — 每回合独立会话无跨回合轮号面（0 即无偏移，R98 语义同 maka）。
func (s *acpSession) turnsContribution() float64 { return 0 }

// exportACPEvents — 回合末把沙箱内 acp-out.jsonl 追加到
// <tasklogDir>/<taskID>/acp-events.jsonl（baton project -acp 的输入；驱动器每回合
// 覆写 /tmp 文件，故逐回合搬运累积）。降级纪律同 maka exportRuntimeEvents：
// 失败仅日志不回滚回合（留存面不得影响主链路）。tasklog 空=禁用。
func (s *acpSession) exportACPEvents() {
	if s.tasklog == "" || s.taskID == "" {
		return
	}
	raw, err := s.execOnce(context.Background(), "cat "+acpOutFile+" 2>/dev/null || true", 60)
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}
	dir := filepath.Join(s.tasklog, s.taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.r.event("warn", "[task %s] acp-events 目录创建失败（投影禁用，诚实降级）: %v", s.taskID, err)
		return
	}
	// 恒追加（驱动器每回合覆写 /tmp 文件；WriteFile 截断会丢前回合——首回合文件
	// 不存在由 O_CREATE 承接）
	f, aerr := os.OpenFile(filepath.Join(dir, "acp-events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if aerr != nil {
		s.r.event("warn", "[task %s] acp-events 追加失败（诚实降级）: %v", s.taskID, aerr)
		return
	}
	defer f.Close()
	if _, werr := f.WriteString(raw); werr != nil {
		s.r.event("warn", "[task %s] acp-events 写入失败（诚实降级）: %v", s.taskID, werr)
	}
}

// errorTail — 失败诊断（尽力而为）：驱动器 stderr 尾。
func (s *acpSession) errorTail(ctx context.Context) string {
	dctx, cancel := context.WithTimeout(ctx, 15e9)
	defer cancel()
	res, err := s.execOnce(dctx, "tail -c 800 "+acpErrFile+" 2>/dev/null || true", 30)
	if err != nil || res == "" {
		return ""
	}
	return strings.TrimSpace(res)
}

// tasklogDir 统一口径沿用 service 层；acp v0 不导出 baton 投影输入（.sse.log 无、
// RuntimeEventLog 无——batonProjectTurn 对本方言自然跳过=诚实降级，联动结论见
// 主仓库 .agent/plans/graph-ui-channels-plan-2026-09-23.md 审查节）。
