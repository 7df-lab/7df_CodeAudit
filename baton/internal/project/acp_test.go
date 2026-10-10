package project

// acp.go 转译器回归：帧映射/白名单金丝雀/多回合幂等。
// 输入语料与 engine acp_session_test 的 golden 帧同源（线格式=官方 ACP v1）。
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeACP(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "acp-events.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTranspileACP_FrameMapping(t *testing.T) {
	path := writeACP(t,
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"opencode","version":"1.2.3"},"agentCapabilities":{"loadSession":false}}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-a1"}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"结论正文。"},"messageId":"m1"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"提示词回显"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-a1","update":{"sessionUpdate":"tool_call","toolCallId":"tc-1","title":"读取 app.py","kind":"read"}}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`,
		`{"_driver":"auto_permission","picked":"allow_once"}`,
	)
	out, as, err := transpileACP(path)
	if err != nil {
		t.Fatalf("转译失败: %v", err)
	}
	if len(as.Parked) != 0 {
		t.Fatalf("白名单内帧被 Parked: %v", as.Parked)
	}
	// auto_permission v0 不投影（对账挂不上真实调用——告警污染；审计在 engine 侧）
	if strings.Contains(out.String(), "approval") {
		t.Errorf("auto_permission 不应投影: %s", out.String())
	}
	s := out.String()
	for _, want := range []string{
		`event: bridge.hello`, `"provider":"acp:opencode"`,
		`event: session.event`, `"type":"assistant/message"`, `结论正文。`,
		`"type":"user/message"`, `提示词回显`,
		`"type":"tool/call"`, `"name":"读取 app.py"`,
		`"type":"turn/end"`, `"kind":"end_turn"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("合成帧缺 %q:\n%s", want, s)
		}
	}
	// 会话路由：update 帧带真实 sessionId
	if !strings.Contains(s, `"sessionId":"sess-a1"`) {
		t.Errorf("sessionId 未透传:\n%s", s)
	}
}

func TestTranspileACP_CanaryWhitelist(t *testing.T) {
	path := writeACP(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"future_update_kind","content":{}}}}`,
		`not json`,
		`{"_driver":"future_note"}`,
	)
	out, as, err := transpileACP(path)
	if err != nil {
		t.Fatalf("转译失败: %v", err)
	}
	if len(as.Parked) != 3 {
		t.Fatalf("金丝雀应 Parked 3 行, got %v", as.Parked)
	}
	if out.Len() != 0 {
		t.Errorf("白名单外帧不应产出:\n%s", out.String())
	}
}

func TestTranspileACP_HelloOnceAndNoiseSilent(t *testing.T) {
	path := writeACP(t,
		`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"maka","version":"0.2"}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"maka","version":"0.2"}}}`, // 重复 hello（多回合文件）
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"plan","plan":[{},{}]}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}}`,
	)
	out, _, err := transpileACP(path)
	if err != nil {
		t.Fatalf("转译失败: %v", err)
	}
	if n := strings.Count(out.String(), "bridge.hello"); n != 1 {
		t.Errorf("hello 应恰一次, got %d", n)
	}
	if strings.Contains(out.String(), "plan") || strings.Contains(out.String(), "available") {
		t.Errorf("过程面噪音泄漏进投影:\n%s", out.String())
	}
}

func TestTranspileACP_StopReasonDefault(t *testing.T) {
	path := writeACP(t, `{"_driver":"turn_done"}`) // stopReason 缺省
	out, _, err := transpileACP(path)
	if err != nil {
		t.Fatalf("转译失败: %v", err)
	}
	if !strings.Contains(out.String(), `"kind":"end_turn"`) {
		t.Errorf("缺省 stopReason 应落 end_turn:\n%s", out.String())
	}
}

// TestProjectRunACP_EndToEnd — -acp 输入走完整 Run 管线（转译→sseframe→投影产物）。
func TestProjectRunACP_EndToEnd(t *testing.T) {
	path := writeACP(t,
		`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"name":"opencode","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"审计任务目标"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"发现一处注入。"},"messageId":"m1"}}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`,
	)
	outRoot := t.TempDir()
	stats, err := Run(Options{
		ACPPath: path, OutRoot: outRoot, Task: "t-acp-1", Goal: "ACP 投影端到端",
		Agent: "acp", BlobThreshold: 32,
	})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if stats.Events == 0 {
		t.Fatalf("零事件投影（管线断链）: %+v", stats)
	}
	// 产物在位（.tasklog/events.jsonl——转译→投影管线闭环的证据面）
	if _, err := os.Stat(filepath.Join(outRoot, ".tasklog", "events.jsonl")); err != nil {
		t.Fatalf("events.jsonl 未产出: %v", err)
	}
}
