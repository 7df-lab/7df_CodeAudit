// acp_session 纯函数面回归：
// parseACPExchange 帧解析（握手/消息块/工具/计划/噪音/代答注记/畸形行容忍）与
// renderTurn 行协议对齐（parseTimeline 前缀兼容）。管道式回合驱动的 exec 面由
// sim/e2e 批实证（镜像 provisioning 前置），单测锁协议语义。
package sandbox

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ===== 批式预言机（2026-09-22 等价简化批自 acp_session.go 移入——生产零调用，
// 仅本文件引用：acpStream 流式渲染的批式等价预言机）=====

// splitACPReadout — 输出文件读取拆分（帧正文 / stderr 尾）。
func splitACPReadout(raw string) (frames, errTail string) {
	if i := strings.Index(raw, "----ERR----"); i >= 0 {
		return raw[:i], strings.TrimSpace(raw[i+11:])
	}
	return raw, ""
}

// acpExchange — 一回合 ACP 交换的解析面（纯函数产出，测试 golden 锁）。
type acpExchange struct {
	SessionID   string
	AgentName   string
	StopReason  string
	FinalText   string   // assistant 消息块拼接（\n\n 连接；text-final 解析输入）
	Humanized   []string // 渲染行（行协议对齐 dsh：✍/▶/⚠/■ 前缀）
	Fatal       string   // 驱动器致命注记（未收敛/握手缺字段）
	AutoPerms   int      // 权限代答次数
	Unsupported int      // 未支持服务端请求数
}

// parseACPExchange — 帧文件 → 交换结构（纯函数；畸形行容忍跳过——留存面同 .sse.log 纪律）。
// 帧面：JSON-RPC 响应（id 1/2/3）、session/update 通知、_driver 注记帧。
func parseACPExchange(frames string) (*acpExchange, error) {
	ex := &acpExchange{}
	var blocks []string
	var cur strings.Builder
	curMsg := any(nil)
	flush := func() {
		if cur.Len() > 0 {
			blocks = append(blocks, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.Split(frames, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue // 畸形行跳过（不致断解析）
		}
		if note, ok := f["_driver"].(string); ok {
			switch note {
			case "turn_done":
				if sr, _ := f["stopReason"].(string); sr != "" {
					ex.StopReason = sr
				}
			case "fatal":
				ex.Fatal = fmt.Sprintf("%v", f["why"])
			case "auto_permission":
				ex.AutoPerms++
				ex.Humanized = append(ex.Humanized, fmt.Sprintf("⚠ 权限请求已代答（选中 %v）——管道式无交互面", f["picked"]))
			case "unsupported_request":
				ex.Unsupported++
				ex.Humanized = append(ex.Humanized, fmt.Sprintf("⚠ 服务端请求未支持已拒答: %v", f["method"]))
			}
			continue
		}
		id, hasID := f["id"].(float64)
		method, _ := f["method"].(string)
		switch {
		case hasID && method == "": // 响应
			res, _ := f["result"].(map[string]any)
			switch int(id) {
			case 1:
				if ai, _ := res["agentInfo"].(map[string]any); ai != nil {
					n, _ := ai["name"].(string)
					v, _ := ai["version"].(string)
					ex.AgentName = strings.TrimSpace(n + " " + v)
				}
			case 2:
				ex.SessionID, _ = res["sessionId"].(string)
				if ex.SessionID == "" {
					return nil, fmt.Errorf("session/new 响应缺 sessionId")
				}
			case 3:
				if sr, _ := res["stopReason"].(string); sr != "" {
					ex.StopReason = sr
				}
			}
		case method == "session/update": // 通知
			params, _ := f["params"].(map[string]any)
			upd, _ := params["update"].(map[string]any)
			if upd == nil {
				continue
			}
			tag, _ := upd["sessionUpdate"].(string)
			switch tag {
			case "agent_message_chunk":
				content, _ := upd["content"].(map[string]any)
				ctype, _ := content["type"].(string)
				if ctype != "text" {
					ex.Humanized = append(ex.Humanized, fmt.Sprintf("▶ [输出块] 非 text 内容（%s）已略", ctype))
					continue
				}
				mid := upd["messageId"]
				if mid != curMsg && curMsg != nil {
					flush()
				}
				curMsg = mid
				if txt, _ := content["text"].(string); txt != "" {
					cur.WriteString(txt)
				}
			case "tool_call":
				title, _ := upd["title"].(string)
				kind, _ := upd["kind"].(string)
				name, _ := upd["toolCallId"].(string)
				ex.Humanized = append(ex.Humanized, fmt.Sprintf("▶ [工具] %s（%s · %s）", title, kind, name))
			case "tool_call_update":
				status, _ := upd["status"].(string)
				if status == "completed" || status == "failed" {
					title, _ := upd["title"].(string)
					ex.Humanized = append(ex.Humanized, fmt.Sprintf("▶ [工具·%s] %s", status, title))
				}
			case "plan":
				if plan, ok := upd["plan"].([]any); ok {
					ex.Humanized = append(ex.Humanized, fmt.Sprintf("▶ [计划] %d 项", len(plan)))
				}
			case "user_message_chunk", "available_commands_update", "current_mode_update", "config_option_update":
				// 机器噪音静默（ADR-181 同口径：我方提示词回显/模式面不渲染）
			default:
				ex.Humanized = append(ex.Humanized, fmt.Sprintf("▶ [未识别 update] %s", tag))
			}
		}
	}
	flush()
	ex.FinalText = strings.Join(blocks, "\n\n")
	return ex, nil
}

// golden 帧样本：一次完整交换（initialize→session/new→update 流→prompt 响应），
// 含两个 messageId 的 assistant 块、工具调用、计划、噪音 update 与驱动器注记。
const acpGoldenFrames = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"opencode","version":"1.2.3"},"agentCapabilities":{"loadSession":false}}}
{"_driver":"auto_permission","picked":"allow_once"}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-a1b2"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"回显略"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"plan","plan":[{"content":"侦察","status":"completed"},{"content":"审计","status":"in_progress"}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"tool_call","toolCallId":"tc-1","title":"读取 app.py","kind":"read"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"分析前半："},"messageId":"m-1"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"发现 SQL 注入。"},"messageId":"m-1"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"tool_call_update","toolCallId":"tc-1","title":"读取 app.py","status":"completed"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"最终结论 JSON 在此。"},"messageId":"m-2"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1b2","update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
{"_driver":"turn_done","stopReason":"end_turn"}
`

func TestParseACPExchange_FullGolden(t *testing.T) {
	ex, err := parseACPExchange(acpGoldenFrames)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if ex.SessionID != "sess-a1b2" {
		t.Errorf("sessionId = %q, want sess-a1b2", ex.SessionID)
	}
	if ex.AgentName != "opencode 1.2.3" {
		t.Errorf("agentName = %q, want opencode 1.2.3", ex.AgentName)
	}
	if ex.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", ex.StopReason)
	}
	wantText := "分析前半：发现 SQL 注入。\n\n最终结论 JSON 在此。"
	if ex.FinalText != wantText {
		t.Errorf("finalText = %q, want %q", ex.FinalText, wantText)
	}
	if ex.AutoPerms != 1 {
		t.Errorf("autoPerms = %d, want 1", ex.AutoPerms)
	}
	if ex.Fatal != "" {
		t.Errorf("fatal = %q, want 空", ex.Fatal)
	}
	joined := strings.Join(ex.Humanized, "\n")
	for _, want := range []string{
		"⚠ 权限请求已代答（选中 allow_once）",
		"▶ [计划] 2 项",
		"▶ [工具] 读取 app.py（read · tc-1）",
		"▶ [工具·completed] 读取 app.py",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("humanized 缺行 %q；实得:\n%s", want, joined)
		}
	}
	// 噪音面静默（ADR-181 同口径）：user_message_chunk 回显与 available_commands 不渲染
	if strings.Contains(joined, "回显略") || strings.Contains(joined, "available") {
		t.Errorf("噪音面泄漏进 humanized:\n%s", joined)
	}
}

func TestParseACPExchange_MalformedLinesTolerated(t *testing.T) {
	in := "not json at all\n\n" +
		`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}` + "\n"
	ex, err := parseACPExchange(in)
	if err != nil {
		t.Fatalf("畸形行应容忍: %v", err)
	}
	if ex.SessionID != "s1" || ex.StopReason != "end_turn" {
		t.Errorf("合法帧未被解析: %+v", ex)
	}
}

func TestParseACPExchange_MissingSessionIDFailFast(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":2,"result":{}}` + "\n"
	if _, err := parseACPExchange(in); err == nil {
		t.Fatal("session/new 缺 sessionId 应 fail-fast")
	}
}

func TestParseACPExchange_DriverFatal(t *testing.T) {
	in := `{"_driver":"fatal","why":"turn not converged (stream end or budget exhausted)"}` + "\n"
	ex, err := parseACPExchange(in)
	if err != nil {
		t.Fatalf("fatal 注记是解析面合法输入: %v", err)
	}
	if ex.Fatal == "" || !strings.Contains(ex.Fatal, "not converged") {
		t.Errorf("fatal = %q", ex.Fatal)
	}
}

func TestParseACPExchange_NonTextContentSkipped(t *testing.T) {
	in := `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"aGk=","mimeType":"image/png"},"messageId":"m-9"}}}` + "\n"
	ex, err := parseACPExchange(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if ex.FinalText != "" {
		t.Errorf("非 text 块不应进 finalText: %q", ex.FinalText)
	}
	if !strings.Contains(strings.Join(ex.Humanized, "\n"), "非 text 内容（image）已略") {
		t.Errorf("非 text 块应留痕略过行: %v", ex.Humanized)
	}
}

// 行协议对齐锁（v0.5 流式）：回合头（turn() 内联）+流式渲染行的前缀必须被 web
// parseTimeline 的 startKind/metaLine 消费（──/📋/✍/■ 与 ▶/⚠ meta 行）。
func TestRenderTurn_LineProtocol(t *testing.T) {
	var sb strings.Builder
	st := &acpStream{emit: func(t string) { sb.WriteString(t) }}
	st.line(`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"opencode","version":"1.2.3"}}}`)
	st.line(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"plan","plan":[{}]}}}`)
	st.line(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"正文"},"messageId":"m1"}}}`)
	st.line(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	out := fmt.Sprintf("── 第 2 轮开始 ──\n📋 [任务下发]（%d 字节）\n提示词\n", len("提示词")) + sb.String()
	for _, prefix := range []string{"── 第 2 轮开始 ──", "📋 [任务下发]（", "▶ [ACP] agent opencode 1.2.3", "✍ [输出]", "■ 回合收束（stopReason=end_turn）"} {
		if !strings.Contains(out, prefix) {
			t.Errorf("renderTurn 缺行协议前缀 %q；实得:\n%s", prefix, out)
		}
	}
}

// splitACPReadout — 帧正文与 stderr 尾拆分（----ERR---- 分隔符契约）。
func TestSplitACPReadout(t *testing.T) {
	frames, errTail := splitACPReadout("frame1\nframe2\n----ERR----\nboom")
	if strings.TrimSpace(frames) != "frame1\nframe2" || errTail != "boom" {
		t.Errorf("split 失常: frames=%q errTail=%q", frames, errTail)
	}
	f2, e2 := splitACPReadout("only frames")
	if f2 != "only frames" || e2 != "" {
		t.Errorf("无分隔符分支失常: %q %q", f2, e2)
	}
}

// ===== v0.5 实时流渲染器（acpStream）=====

// streamGolden — 喂同一份 golden 帧，流式渲染 vs 批式解析双锁。
func streamGoldenLines(t *testing.T) []string {
	t.Helper()
	var out []string
	st := &acpStream{emit: func(s string) { out = append(out, s) }}
	for _, ln := range strings.Split(acpGoldenFrames, "\n") {
		st.line(ln)
	}
	_ = st.finalText()
	return out
}

func TestACPStream_RealtimeEmission(t *testing.T) {
	lines := streamGoldenLines(t)
	joined := strings.Join(lines, "")
	for _, want := range []string{
		"▶ [ACP] agent opencode 1.2.3",
		"✍ [输出]",
		"分析前半：", // chunk 到即出（实时性锁：不待回合末）
		"▶ [计划] 2 项",
		"▶ [工具] 读取 app.py",
		"■ 回合收束（stopReason=end_turn）",
		"⚠ 权限请求已代答（选中 allow_once）",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("流式渲染缺 %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "回显略") {
		t.Errorf("噪音面泄漏（user_message_chunk 应静默）:\n%s", joined)
	}
}

func TestACPStream_FinalTextEquivalence(t *testing.T) {
	// 批式解析器=等价预言机：流式累积的 finalText 与批式 FinalText 必须一致
	st := &acpStream{emit: func(string) {}}
	for _, ln := range strings.Split(acpGoldenFrames, "\n") {
		st.line(ln)
	}
	batch, err := parseACPExchange(acpGoldenFrames)
	if err != nil {
		t.Fatalf("批式解析: %v", err)
	}
	if got := st.finalText(); got != batch.FinalText {
		t.Errorf("finalText 分叉:\n流式=%q\n批式=%q", got, batch.FinalText)
	}
	if st.stop != batch.StopReason {
		t.Errorf("stopReason 分叉: 流式=%q 批式=%q", st.stop, batch.StopReason)
	}
}

func TestACPStream_MessageIdBlockSplit(t *testing.T) {
	st := &acpStream{emit: func(string) {}}
	frames := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"块一"},"messageId":"m1"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"续"},"messageId":"m1"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"块二"},"messageId":"m2"}}}`,
	}
	for _, f := range frames {
		st.line(f)
	}
	if got := st.finalText(); got != "块一续\n\n块二" {
		t.Errorf("messageId 分段失常: %q", got)
	}
}

func TestACPStream_MalformedAndPartialLinesTolerated(t *testing.T) {
	var n int
	st := &acpStream{emit: func(string) { n++ }}
	st.line("半行不完整 fra") // 无换行完整性由尾读层保证;这里锁不 panic
	st.line("")
	st.line("not json")
	if n != 0 {
		t.Errorf("畸形行不应产出: %d", n)
	}
}
