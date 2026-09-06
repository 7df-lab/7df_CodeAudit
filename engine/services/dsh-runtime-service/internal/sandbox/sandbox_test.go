package sandbox

// ADR-168 回归锁：经 openshell-manager 微服务 + 沙箱内 bridge（JSON-RPC⇄SSE）的
// 生命周期契约（httptest 假 manager + 假 bridge，无需网关）/ SSE 帧解析 / 结果解析
// fence 容错 / mode=off 哨兵 / 恒 teardown / Bearer 鉴权头 / 交互日志原始帧外流。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseFindings_FenceTolerant(t *testing.T) {
	cases := map[string]string{
		"fenced":     "分析如下\n```json\n{\"findings\":[{\"title\":\"x\",\"start_line\":3,\"confidence\":0.8}]}\n```\n",
		"plain":      "结论：{\"findings\":[{\"title\":\"y\"}]}\n",
		"with-noise": "```json\n{\"findings\": []}\n```\n额外说明",
	}
	for name, out := range cases {
		fs, err := ParseFindings(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "fenced" && (len(fs) != 1 || fs[0].Title != "x" || fs[0].StartLine != 3) {
			t.Fatalf("%s: parsed %+v", name, fs)
		}
	}
	if _, err := ParseFindings("no json here"); err == nil {
		t.Fatal("no-JSON output must error")
	}
}

func TestRunner_DisabledSentinel(t *testing.T) {
	r := NewManagerRunner(Config{Mode: "off"})
	_, err := r.Run(context.Background(), Task{WorkspaceDir: ".", Assignment: "x"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("mode=off must yield ErrDisabled, got %v", err)
	}
	if r.Enabled() {
		t.Fatal("mode=off must not be enabled")
	}
}

// fakeBridge — 沙箱内 bridge（CD/dsh-pentest-sse/bridge.mjs）HTTP 面的最小假实现：
// GET /events SSE 订阅 + POST /prompt 触发脚本化事件序列。
type fakeBridge struct {
	mu          sync.Mutex
	promptText  string
	sub         http.ResponseWriter // 当前 SSE 订阅者（单订阅者即可）
	subCtx      context.Context
	script      []string   // prompt 后依序写出的原始 SSE 帧（已含 \n\n 分帧）
	scripts     [][]string // ADR-190：多段脚本（第 N 次 prompt 回放第 N 段；空=用 script）
	flushed     bool
	promptCount int
}

func (f *fakeBridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "retry: 2000\n\n")
		fmt.Fprint(w, "event: bridge.hello\ndata: {\"provider\":\"deepseek-official\",\"model\":\"deepseek-v4-flash\",\"agentCwd\":\"/sandbox\",\"filter\":null}\n\n")
		flusher.Flush()
		f.mu.Lock()
		f.sub = w
		f.subCtx = r.Context()
		f.mu.Unlock()
		<-r.Context().Done() // 订阅保持到客户端断开
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.promptText = body.Text
		f.promptCount++
		script := append([]string(nil), f.script...)
		if len(f.scripts) > 0 { // ADR-190：多段脚本按 prompt 次序取（越界取末段）
			idx := f.promptCount - 1
			if idx >= len(f.scripts) {
				idx = len(f.scripts) - 1
			}
			script = append([]string(nil), f.scripts[idx]...)
		}
		f.mu.Unlock()
		go func() {
			time.Sleep(100 * time.Millisecond) // 订阅先于事件（与真实流一致）
			for i, frame := range script {
				f.mu.Lock()
				sub, ctx := f.sub, f.subCtx
				f.mu.Unlock()
				if sub == nil || ctx == nil || ctx.Err() != nil {
					return
				}
				fmt.Fprint(sub, frame)
				if f, ok := sub.(http.Flusher); ok {
					f.Flush()
				}
				_ = i
			}
		}()
		fmt.Fprintf(w, `{"sessionId":%q,"messageId":"mid-1"}`, body.SessionID)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"runtime":"down"}`)
	})
	return mux
}

// frames_Success — 正常收敛脚本：running → assistant/message → turn/end → idle。
func frames_Success(findingsJSON string) []string {
	ev := func(typ, data string) string {
		return "event: session.event\ndata: " + data + "\n\n"
	}
	_ = ev
	assistant := `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"` + jsEscape(findingsJSON) + `"}]}}}}`
	return []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + assistant + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":30,\"data\":{\"turn\":1,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
		": ping\n\n",
	}
}

// frames_TurnError — 回合错误脚本（provider 拒绝等）：turn/end reason=error → idle。
func frames_TurnError() []string {
	return []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":30,\"data\":{\"turn\":1,\"reason\":{\"kind\":\"error\",\"error\":{\"message\":\"max_tokens参数非法\",\"code\":\"INVALID_REQUEST\",\"status\":400}}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
}

func jsEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// fakeManager — openshell-manager 路由契约的最小假实现（http_api.py ROUTES 子集，
// ADR-168 bridge 通道形状：create/wait-ready/exec/services/files/delete）。
type fakeManager struct {
	mu           sync.Mutex
	token        string
	created      map[string]bool
	deleted      []string
	execCount    int
	launchScript string
	bridgeURL    string
	failLaunch   bool
	uploadCount  int    // ADR-187：项目 tar.gz 上传（POST files）次数
	uploadPath   string // 最近一次上传的 path 字段
}

func (f *fakeManager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(r) {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"error":"unauthorized"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		f.mu.Lock()
		if f.created == nil {
			f.created = map[string]bool{}
		}
		f.created[name] = true
		f.mu.Unlock()
		spec, _ := body["spec"].(map[string]any)
		policy, _ := spec["policy"].(map[string]any)
		if v, _ := policy["version"].(float64); v != 1 {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":"policy version must be 1"}`)
			return
		}
		fmt.Fprintf(w, `{"id":"sbx-%s","name":%q,"workspace":%q,"phase":2,"phase_name":"PHASE_READY"}`,
			name, name, body["workspace"])
	})
	mux.HandleFunc("/api/v1/sandboxes/exec", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(r) {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"error":"unauthorized"}`)
			return
		}
		var body struct {
			SandboxID string   `json:"sandbox_id"`
			Command   []string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		cmdline := ""
		if len(body.Command) >= 3 {
			cmdline = body.Command[2]
		}
		if strings.Contains(cmdline, "bridge.mjs") {
			f.execCount++
			f.launchScript = cmdline
			if f.failLaunch {
				fmt.Fprintf(w, `{"exit_code":1,"stdout":"bridge-not-ready","stderr":""}`)
				return
			}
			fmt.Fprintf(w, `{"exit_code":0,"stdout":"{\"ok\":true,\"runtime\":\"down\"}","stderr":""}`)
			return
		}
		// bridgeLogTail 诊断调用（失败路径）
		fmt.Fprintf(w, `{"exit_code":0,"stdout":"bridge: dsh runtime exited","stderr":""}`)
	})
	mux.HandleFunc("/api/v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(r) {
			w.WriteHeader(401)
			fmt.Fprintf(w, `{"error":"unauthorized"}`)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sandboxes/")
		switch {
		case r.Method == "DELETE":
			f.mu.Lock()
			f.deleted = append(f.deleted, rest)
			f.mu.Unlock()
			fmt.Fprintf(w, `{"deleted":true}`)
		case strings.HasSuffix(rest, "/wait-ready"):
			fmt.Fprintf(w, `{"ok":true}`)
		case strings.HasSuffix(rest, "/files"):
			// ADR-187：项目 tar.gz 整包上传端点（multipart：path 字段 + file）
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				fmt.Fprintf(w, `{"error":"method not allowed"}`)
				return
			}
			path := ""
			if mr, merr := r.MultipartReader(); merr == nil {
				for {
					p, perr := mr.NextPart()
					if perr != nil {
						break
					}
					if p.FormName() == "path" {
						b, _ := io.ReadAll(p)
						path = string(b)
					}
				}
			}
			f.mu.Lock()
			f.uploadCount++
			f.uploadPath = path
			f.mu.Unlock()
			fmt.Fprintf(w, `{"ok":true}`)
		case strings.HasSuffix(rest, "/services"):
			name := strings.TrimSuffix(rest, "/services")
			fmt.Fprintf(w, `{"name":"bridge","sandbox_id":"uuid-1","sandbox_name":%q,"target_port":8080,"url":%q}`,
				name, f.bridgeURL)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	})
	return mux
}

func (f *fakeManager) auth(r *http.Request) bool {
	return f.token == "" || r.Header.Get("Authorization") == "Bearer "+f.token
}

func newTestWorkspace(t *testing.T) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

const stubFindings = "```json\n{\"findings\":[{\"title\":\"stub\",\"severity\":\"SEVERITY_HIGH\",\"cwe_id\":\"CWE-89\",\"file_path\":\"a.py\",\"start_line\":1,\"confidence\":0.9,\"description\":\"d\",\"reasoning\":\"r\"}]}\n```"

func TestRun_FullLifecycleViaBridge(t *testing.T) {
	fb := &fakeBridge{script: frames_Success(stubFindings)}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()

	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	var human, raw syncwriter
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.2",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		// 测试中沙箱服务域无 DNS：显式把服务路由拨号指到假 bridge（等价生产网关直拨）
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		OnHumanLog:      human.writeString,
		OnRawLog:        raw.write,
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "task-1", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK || len(res.Findings) != 1 || res.Findings[0].Title != "stub" {
		t.Fatalf("result: %+v", res)
	}
	if res.Meta.ExitCode != 0 || res.Meta.SubagentID == "" {
		t.Fatalf("meta: %+v", res.Meta)
	}
	// 任务经 /prompt 文本体通道进 bridge：路径告知式（ADR-187）——含任务指令与
	// 项目沙箱路径，不含任何工作区文件内容（代码走 files 端点整包上传）
	fb.mu.Lock()
	prompt := fb.promptText
	n := fb.promptCount
	fb.mu.Unlock()
	if n != 1 || prompt == "" || !strings.Contains(prompt, "审计它") ||
		!strings.Contains(prompt, ProjectSandboxPath) {
		t.Fatalf("prompt missing parts: n=%d head=%.120s", n, prompt)
	}
	if strings.Contains(prompt, "print('x')") || strings.Contains(prompt, "a.py") {
		t.Fatalf("inline-prompt regression: workspace file content leaked into prompt:\n%.300s", prompt)
	}
	// 项目 tar.gz 确经 manager files 端点上传（ADR-187：上传替代内联）
	fm.mu.Lock()
	upN, upPath := fm.uploadCount, fm.uploadPath
	fm.mu.Unlock()
	if upN != 1 || upPath != "/tmp/am-project.tar.gz" {
		t.Fatalf("project upload: n=%d path=%q", upN, upPath)
	}

	// bridge 拉起脚本：maxTokens 覆盖 + inference.local 占位凭据（凭据不进沙箱纪律）
	// + DSH_PERMISSION_MODE（ADR-187 补遗：DSH 内层收禁后端容器内缺失，须跳过）
	fm.mu.Lock()
	script, execN := fm.launchScript, fm.execCount
	fm.mu.Unlock()
	if execN != 1 {
		t.Fatalf("exec calls = %d, want 1 (bridge launch)", execN)
	}
	if !strings.Contains(script, "DSH_MAX_TOKENS=131072") ||
		!strings.Contains(script, "DSH_PERMISSION_MODE=danger-full-access") ||
		!strings.Contains(script, "DEEPSEEK_BASE_URL=https://inference.local/v1") ||
		!strings.Contains(script, "DEEPSEEK_API_KEY=openshell-injected") {
		t.Fatalf("launch script: %.200s", script)
	}
	// AI 交互日志双流：人性化流面向用户（含会话头与收束行），原始帧仅落盘通道外流
	if got := human.String(); !strings.Contains(got, "DSH 会话开始") || !strings.Contains(got, "回合结束") {
		t.Fatalf("humanized stream incomplete: %.200s", got)
	}
	if got := human.String(); strings.Contains(got, "\"sessionId\"") {
		t.Fatal("humanized stream must not dump raw JSON frames")
	}
	if got := raw.String(); !strings.Contains(got, "event: bridge.hello") {
		t.Fatalf("raw stream incomplete: %.200s", got)
	}
	// 恒 teardown：成功路径也要删沙箱；沙箱名 ≤ 网关服务路由 19 字符上限
	if len(fm.deleted) != 1 || len(fm.created) != 1 {
		t.Fatalf("created=%v deleted=%v", fm.created, fm.deleted)
	}
	for name := range fm.created {
		if fm.deleted[0] != name {
			t.Fatalf("deleted %q != created %q", fm.deleted[0], name)
		}
		if len(name) > 19 {
			t.Fatalf("sandbox name %q exceeds gateway 19-char cap", name)
		}
	}
}

// ADR-190 回归锁①：子任务会话的 idle 不是回合终态——gw-391b10f1 实证（4 个后台
// 子代理之一被 provider 断流掐死，其 idle 曾被误判为主会话收敛 → 主会话尚未产出
// submit_findings 即拆沙箱 → "no JSON in DSH output"）。修复后：子 idle 滤除，
// 主会话继续产出并正常收敛。
func TestRun_SubagentIdleDoesNotConverge(t *testing.T) {
	fb := &fakeBridge{script: []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		// 子任务会话 idle（修复前：此处触发提前收敛）
		"event: session.status\ndata: {\"sessionId\":\"bb859dbe-8be0-418a-83de-c8442ba96e90\",\"status\":\"idle\"}\n\n",
		// 主会话在此之后继续产出（修复前已丢失）
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"` + jsEscape(stubFindings) + `"}]}}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":30,\"data\":{\"turn\":1,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-sub-idle", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v (修复前此处即 'no JSON in DSH output')", err)
	}
	if !res.OK || len(res.Findings) != 1 || res.Findings[0].Title != "stub" {
		t.Fatalf("result: %+v", res)
	}
	fb.mu.Lock()
	n := fb.promptCount
	fb.mu.Unlock()
	if n != 1 {
		t.Fatalf("prompt count = %d, want 1 (no recovery needed)", n)
	}
}

// ADR-190 回归锁②：子任务推理流中断（turn/end error）+ 主会话空闲且未提交 →
// 恢复指令恰一轮驱动模型经 submit_findings 提交（失败反馈自纠同精神）。
func TestRun_DeadSubagentRecoveryRound(t *testing.T) {
	subErr := "event: session.event\ndata: " +
		`{"sessionId":"bb859dbe-8be0-418a-83de-c8442ba96e90","event":{"type":"turn/end","seq":99,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"SSE stream ended without [DONE]","code":"STREAM_CLOSED"}}}}}` + "\n\n"
	seg1 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		subErr, // 子任务死亡
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"让我检查集群订阅的实现……"}]}}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":30,\"data\":{\"turn\":1,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	const submitArgs = `{"findings":[{"title":"stub","severity":"SEVERITY_HIGH","cwe_id":"CWE-89","file_path":"a.py","start_line":1,"confidence":0.9,"description":"d","reasoning":"r"}]}`
	seg2 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":40,\"data\":{\"turn\":2}}}\n\n",
		`event: session.event` + "\n" + `data: {"sessionId":"main","event":{"type":"tool/call","seq":50,"data":{"turn":2,"step":1,"callId":"c-1","name":"submit_findings","arguments":"` + jsEscape(submitArgs) + `"}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":60,"data":{"message":{"content":[{"type":"text","text":"已提交。"}]}}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":70,\"data\":{\"turn\":2,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{scripts: [][]string{seg1, seg2}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	var events logwriter
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-sub-dead", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK || len(res.Findings) != 1 || res.Findings[0].Title != "stub" {
		t.Fatalf("result: %+v (submit_findings 通道应解析出发现)", res)
	}
	fb.mu.Lock()
	n, last := fb.promptCount, fb.promptText
	fb.mu.Unlock()
	if n != 2 {
		t.Fatalf("prompt count = %d, want 2 (初始 + 恢复恰一轮)", n)
	}
	if !strings.Contains(last, "推理流中断") {
		t.Fatalf("second prompt must be the recovery directive: %.120s", last)
	}
	if !strings.Contains(events.String(), "恢复指令") {
		t.Fatalf("recovery event log missing:\n%s", events.String())
	}
}

// logwriter — EventFn 最小汇（并发安全）。
type logwriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *logwriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.WriteString(s)
}

func (w *logwriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// 端到端回归（gw-5a96f1f7）：模型按双契约调用 submit_findings 提交空列表 + 纯文本
// 总结收敛 → Run 必须成功且零发现（此前被误报 "no JSON in DSH output"，整轮报废、
// 上游降级 RuleScan——一次完整健康的审计被丢弃）。
func TestRun_EmptyFindingsViaToolSucceeds(t *testing.T) {
	const emptySubmit = `{"findings": []}`
	seg := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		`event: session.event` + "\n" + `data: {"sessionId":"main","event":{"type":"tool/call","seq":20,"data":{"turn":1,"step":1,"callId":"c-1","name":"submit_findings","arguments":"` + jsEscape(emptySubmit) + `"}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":30,"data":{"message":{"content":[{"type":"text","text":"审计完成，未发现可利用的安全漏洞，已按输出契约提交空 findings 列表。"}]}}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":40,\"data\":{\"turn\":1,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{script: seg}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	var events logwriter
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-empty", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK || len(res.Findings) != 0 {
		t.Fatalf("clean zero must succeed: ok=%v findings=%d", res.OK, len(res.Findings))
	}
	ev := events.String()
	if !strings.Contains(ev, "findings=0") || !strings.Contains(ev, "通道=submit_findings") {
		t.Fatalf("success event must record zero findings via tool channel:\n%s", ev)
	}
	if strings.Contains(ev, "解析失败") {
		t.Fatalf("must not report parse failure for clean zero:\n%s", ev)
	}
}

// syncwriter — 并发安全的字节汇（OnAILog 从流 goroutine 调用）。
type syncwriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncwriter) write(p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
}

func (w *syncwriter) writeString(s string) { w.write([]byte(s)) }

func (w *syncwriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestRun_TeardownEvenOnTurnError(t *testing.T) {
	fb := &fakeBridge{script: frames_TurnError()}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()

	fm := &fakeManager{bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL,
		Workspace: "w", Image: "dsh-pentest-sse:1.0.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	})
	res, err := r.Run(context.Background(), Task{TaskID: "t", WorkspaceDir: newTestWorkspace(t), Assignment: "x"})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("turn error must surface with provider message, got %v", err)
	}
	if len(fm.deleted) != 1 {
		t.Fatalf("sandbox must be deleted on failure, deleted=%v", fm.deleted)
	}
	if res.Error == "" {
		t.Fatal("Result.Error must carry the failure honestly")
	}
}

func TestRun_TeardownEvenOnLaunchFailure(t *testing.T) {
	fm := &fakeManager{failLaunch: true}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL,
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 5, ExecTimeoutS: 30,
	})
	if _, err := r.Run(context.Background(), Task{TaskID: "t", WorkspaceDir: newTestWorkspace(t), Assignment: "x"}); err == nil {
		t.Fatal("bridge launch failure must surface")
	}
	if len(fm.deleted) != 1 {
		t.Fatalf("sandbox must be deleted on failure, deleted=%v", fm.deleted)
	}
}

func TestRun_UnreachableManagerFailsLoud(t *testing.T) {
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: "http://127.0.0.1:1", // 无监听端口
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 1, ExecTimeoutS: 1,
	})
	if _, err := r.Run(context.Background(), Task{WorkspaceDir: newTestWorkspace(t), Assignment: "x"}); err == nil {
		t.Fatal("unreachable manager must fail loud (no direct-gRPC fallback)")
	}
}

func TestRun_BearerTokenRequired(t *testing.T) {
	fm := &fakeManager{token: "secret"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "wrong",
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 1, ExecTimeoutS: 1,
	})
	if _, err := r.Run(context.Background(), Task{WorkspaceDir: newTestWorkspace(t), Assignment: "x"}); err == nil {
		t.Fatal("401 must surface as error")
	}
}

// TestSSEParser_BridgeFrames — bridge SSE 帧行解析器（wire 契约 bridge.mjs 同源）+ 人性化渲染。
// ADR-181 回归锁：模型路由/审批策略/系统上下文/用量/tool-call 静默；任务下发全文；
// 子任务会话只出骨架行且不参与收敛投影（gw-3b0b9ebf 乱码根因）。
func TestSSEParser_BridgeFrames(t *testing.T) {
	var human syncwriter
	p := &sseParser{onHuman: human.writeString}
	feed := func(lines ...string) (evs []bridgeEvent) {
		for _, l := range lines {
			if ev := p.line([]byte(l + "\n")); ev != nil {
				evs = append(evs, *ev)
			}
		}
		return evs
	}
	// 心跳与未知事件忽略；会话头成行
	if evs := feed(": ping\n", "\n",
		"event: bridge.hello\n", "data: {\"provider\":\"deepseek-official\",\"model\":\"m1\",\"agentCwd\":\"/sandbox\"}\n", "\n"); len(evs) != 0 {
		t.Fatalf("hello/heartbeat must not project, got %+v", evs)
	}
	feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"permission/preset\",\"data\":{\"preset\":\"workspace-write\"}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"approval/policy\",\"data\":{\"policy\":\"ask\"}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"user/message\",\"data\":{\"content\":[{\"type\":\"text\",\"text\":\"# CodeAudit 代码安全分析任务\\n## 代码范围\\n## 任务 全文在此\"}]}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"user/message\",\"data\":{\"content\":[{\"type\":\"text\",\"text\":\"<system-reminder>内部提醒</system-reminder>\"}]}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"request/header\",\"data\":{\"header\":{\"config\":{\"provider\":\"p\",\"model\":\"m1\",\"maxTokens\":131072,\"reasoningEffort\":\"high\"}}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":1}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"block-start\",\"blockType\":\"reasoning\"}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"reasoning-delta\",\"text\":\"Let me analyze\"}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"block-end\",\"block\":{\"type\":\"reasoning\"}}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"block-start\",\"blockType\":\"tool-call\"}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"tool-call-delta\",\"text\":\"{\\\"cmd\\\":\"}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"block-end\",\"block\":{\"type\":\"tool-call\",\"name\":\"bash\"}}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"usage\",\"usage\":{\"inputTokens\":992,\"outputTokens\":530,\"reasoningTokens\":392}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"text-delta\",\"text\":\"结论\"}}}}\n", "\n")
	if !strings.Contains(human.String(), "Let me analyze") {
		t.Fatalf("reasoning delta must stream verbatim")
	}
	// running 状态不收敛且静默
	if evs := feed("event: session.status\n", "data: {\"sessionId\":\"main\",\"status\":\"running\"}\n", "\n"); len(evs) != 0 {
		t.Fatalf("running status must not project, got %+v", evs)
	}
	// 子任务会话：骨架行（启动+任务全文）但不流式正文、不产生收敛投影；
	// ADR-190 例外——turn/end reason=error 投影 agentErr 死亡信号（不误伤整体：
	// err/idle/assistantText 仍不产生）
	evs := feed("event: session.event\n",
		"data: {\"sessionId\":\"4bb5a865-791c-4b67-811a-d56a7aec3a41\",\"event\":{\"type\":\"agent/inbox/spliced\",\"data\":{\"target\":\"next-turn\",\"inserted\":[{\"content\":[{\"type\":\"text\",\"text\":\"你是资深渗透测试员，正在对 Java 项目做白盒审计\"}]}]}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"4bb5a865-791c-4b67-811a-d56a7aec3a41\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"text-delta\",\"text\":\"子任务正文不得混入\"}}}}\n", "\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"4bb5a865-791c-4b67-811a-d56a7aec3a41\",\"event\":{\"type\":\"turn/end\",\"data\":{\"reason\":{\"kind\":\"error\",\"error\":{\"message\":\"子任务错误不得误伤主回合\"}}}}}\n", "\n",
		// ADR-190：子任务 idle 不是回合终态（gw-391b10f1 回归）
		"event: session.status\n",
		"data: {\"sessionId\":\"4bb5a865-791c-4b67-811a-d56a7aec3a41\",\"status\":\"idle\"}\n", "\n")
	if len(evs) != 1 || evs[0].agentErr == "" || evs[0].idle || evs[0].err != nil || evs[0].assistantText != "" {
		t.Fatalf("subagent events: want exactly one agentErr death signal (no idle/err/assistantText), got %+v", evs)
	}
	if !strings.Contains(human.String(), "❌ 推理流中断") && !strings.Contains(human.String(), "子任务错误不得误伤主回合") {
		t.Fatalf("dead subagent must render a human line:\n%s", human.String())
	}
	// 子任务回报（主会话收件）：完整正文
	feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"user/message\",\"data\":{\"content\":[{\"type\":\"text\",\"text\":\"Background subagent 4bb5a865 reported:mica-mqtt 审计结论：共 3 项发现\"}]}}}\n", "\n")
	// assistant 文本投影（仅主会话）
	evs = feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"```json\\n{\\\"findings\\\":[]}\\n```\"}]}}}}\n",
		"\n")
	if len(evs) != 1 || !strings.Contains(evs[0].assistantText, "findings") {
		t.Fatalf("assistant projection: %+v", evs)
	}
	// turn 错误投影（主会话）
	evs = feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"data\":{\"reason\":{\"kind\":\"error\",\"error\":{\"message\":\"boom\"}}}}}\n",
		"\n")
	if len(evs) != 1 || evs[0].err == nil || !strings.Contains(evs[0].err.Error(), "boom") {
		t.Fatalf("turn error projection: %+v", evs)
	}
	// idle 收敛
	evs = feed("event: session.status\n", "data: {\"sessionId\":\"main\",\"status\":\"idle\"}\n", "\n")
	if len(evs) != 1 || !evs[0].idle {
		t.Fatalf("idle projection: %+v", evs)
	}
	// runtime.exit 错误投影
	evs = feed("event: runtime.exit\n", "data: \"spawn failed\"\n", "\n")
	if len(evs) != 1 || evs[0].err == nil {
		t.Fatalf("runtime.exit projection: %+v", evs)
	}
	// 人性化渲染断言（ADR-181）：保留回合/思考/输出/任务全文/子任务骨架/收束
	h := human.String()
	for _, want := range []string{
		"DSH 会话开始", "💭 [思考]", "✍ [输出]", "── 第 1 轮开始 ──",
		"回合结束: ❌ 错误", "■ 会话空闲",
		"📋 [任务下发]", "## 任务 全文在此",
		"🤖 [子任务 4bb5a865] 启动", "🤖 [子任务 4bb5a865] 任务", "你是资深渗透测试员，正在对 Java 项目做白盒审计",
		"📋 [子任务回报]", "共 3 项发现",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("humanized stream missing %q; got:\n%.600s", want, h)
		}
	}
	// 噪音与串扰断言：元事件静默、子任务正文不得混入、无原始 JSON 倾倒
	for _, banned := range []string{
		"[模型路由]", "[审批策略]", "[系统上下文]", "[用量]", "[tool-call]", "tool_use",
		"子任务正文不得混入", "\"sessionId\"", "agent/inbox",
	} {
		if strings.Contains(h, banned) {
			t.Fatalf("humanized stream leaked %q:\n%.400s", banned, h)
		}
	}
}

func TestBuildTurnPrompt_PathBasedNoInline(t *testing.T) {
	prompt := buildTurnPrompt(Task{Assignment: "对项目做安全审计"})
	for _, want := range []string{ProjectSandboxPath, "对项目做安全审计", "*** Update File:", "submit_findings"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("turn prompt missing %q:\n%s", want, prompt)
		}
	}
	// 防回改退化（ADR-187）：代码全文不进 prompt——内联式措辞与代码清单段必须绝迹
	for _, banned := range []string{"无需读盘", "待审计代码（相对路径 + 全文）", "### "} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("inline-prompt regression: %q present:\n%.400s", banned, prompt)
		}
	}
	// 路径告知式 prompt 与项目体积无关，模板自身有界
	if len(prompt) > 16<<10 {
		t.Fatalf("turn prompt unexpectedly large: %d bytes", len(prompt))
	}
}

func TestSharedConfig_TokenFileResolution(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"url":"http://127.0.0.1:19999","tokenFile":".tok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tok"), []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewManagerRunner(Config{ManagerConfig: filepath.Join(dir, "config.json")})
	url, token := r.managerEndpoint()
	if url != "http://127.0.0.1:19999" || token != "abc123" {
		t.Fatalf("shared config resolution: url=%q token=%q", url, token)
	}
}

// TestLive_ManagerSandboxLifecycle — 真实接入证据（外部依赖，证据纪律见 AGENTS.md ④）：
//
//	CODEAUDIT_SANDBOX_LIVE=smoke  走真 manager 建沙箱→exec echo→删（不依赖网关镜像）
//	CODEAUDIT_SANDBOX_LIVE=full   完整 Run()=真沙箱内 DSH bridge 语义分析（需网关+镜像就绪）
//
// 未设该变量时跳过；token/地址经 OPENSHELL_MANAGER_URL/TOKEN 或共享 config.json 解析。
func TestLive_ManagerSandboxLifecycle(t *testing.T) {
	mode := os.Getenv("CODEAUDIT_SANDBOX_LIVE")
	if mode == "" {
		t.Skip("live gate: set CODEAUDIT_SANDBOX_LIVE=smoke|full")
	}
	cfg := Config{
		Mode: "openshell",
		// 测试 cwd=包目录（internal/sandbox）：上溯 4 级=仓库根 codeaudit，
		// 再上 1 级即 openshell-manager 兄弟目录（部署布局）
		ManagerConfig: filepath.Join("..", "..", "..", "..", "..", "openshell-manager", "config.json"),
		Workspace:     "default", Image: "dsh-pentest-sse:1.2.0", // 现役配置口径（configs/codeaudit.yaml；1.2.0=ADR-185 submit 工具层）
		WaitReadyTimeoutS: 120, ExecTimeoutS: 300, DSHMaxTokens: 131072,
	}
	r := NewManagerRunner(cfg)
	url, token := r.managerEndpoint()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if mode == "smoke" {
		name := "am-" + randomHex12()
		ref, err := r.call(ctx, "POST", url+"/api/v1/sandboxes", token, map[string]any{
			"workspace": cfg.Workspace, "name": name, "spec": sandboxSpec(name, "live-smoke", cfg.Image),
		})
		if err != nil {
			t.Fatalf("live create: %v", err)
		}
		sb := sandboxRefFrom(ref)
		t.Logf("live sandbox created: id=%s name=%s phase=%s", sb.ID, sb.Name, sb.PhaseName)
		defer r.call(context.Background(), "DELETE",
			url+"/api/v1/sandboxes/"+sb.Name+"?workspace="+cfg.Workspace, token, nil)
		if _, err := r.call(ctx, "POST", url+"/api/v1/sandboxes/"+sb.Name+"/wait-ready", token,
			map[string]any{"workspace": cfg.Workspace, "timeout_seconds": 120}); err != nil {
			t.Fatalf("live wait-ready: %v", err)
		}
		er, err := r.call(ctx, "POST", url+"/api/v1/sandboxes/exec", token, map[string]any{
			"sandbox_id": sb.ID, "command": []string{"/bin/echo", "codeaudit-live-ok"},
		})
		if err != nil {
			t.Fatalf("live exec: %v", err)
		}
		if stdout, _ := er["stdout"].(string); stdout != "codeaudit-live-ok\n" {
			t.Fatalf("live exec stdout = %q", stdout)
		}
		t.Logf("live exec exit_code=%v stdout=%q", er["exit_code"], er["stdout"])
		return
	}

	// full: 真实沙箱内 DSH bridge 语义分析（默认玩具样例；CODEAUDIT_SANDBOX_WS 可指定真实语料）
	var logged syncwriter
	cfg.OnHumanLog = logged.writeString
	r = NewManagerRunner(cfg)
	ws := newTestWorkspace(t)
	if p := os.Getenv("CODEAUDIT_SANDBOX_WS"); p != "" {
		ws = p
	}
	res, err := r.Run(ctx, Task{
		TaskID: "live-full", WorkspaceDir: ws,
		Assignment: "对项目做安全审计：识别可被利用的漏洞，按输出契约返回 findings。",
		Timeout:    4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("live full Run: %v (meta=%+v)", err, res.Meta)
	}
	t.Logf("live full run ok: findings=%d exit=%d aiLogBytes=%d",
		len(res.Findings), res.Meta.ExitCode, len(logged.String()))
	for _, f := range res.Findings {
		t.Logf("  finding: %s (%s) %s:%d conf=%.2f", f.Title, f.Severity, f.FilePath, f.StartLine, f.Confidence)
	}
}

// ---- ADR-185：结构化提交通道（submit_findings/submit_patches 工具参数优先） ----

func TestParseAuditResultToolCallPreferred(t *testing.T) {
	args := `{"findings": [{"title": "SQL 注入", "file_path": "a.py"}]}`
	calls := []ToolCall{
		{Name: "read", Arguments: `{"x":1}`},
		{Name: SubmitFindingsTool, Arguments: args},
	}
	fs, ch, err := parseAuditResult("正文无关", calls)
	if err != nil || ch != SubmitFindingsTool || len(fs) != 1 || fs[0].Title != "SQL 注入" {
		t.Fatalf("tool channel must win: ch=%s err=%v n=%d", ch, err, len(fs))
	}
	// 工具参数损坏 → 围栏降级
	fence := "```json\n{\"findings\": [{\"title\": \"降级项\"}]}\n```"
	fs, ch, err = parseAuditResult(fence, []ToolCall{{Name: SubmitFindingsTool, Arguments: "{broken"}})
	if err != nil || ch != "text-fence" || len(fs) != 1 || fs[0].Title != "降级项" {
		t.Fatalf("fence fallback must engage: ch=%s err=%v", ch, err)
	}
	// 未调用工具 → 围栏
	fs, ch, err = parseAuditResult(fence, nil)
	if err != nil || ch != "text-fence" || len(fs) != 1 {
		t.Fatalf("plain fence: ch=%s err=%v", ch, err)
	}
	// 多次 submit_findings 取最后一次
	calls2 := []ToolCall{
		{Name: SubmitFindingsTool, Arguments: `{"findings": []}`},
		{Name: SubmitFindingsTool, Arguments: `{"findings": [{"title": "末次"}]}`},
	}
	fs, _, err = parseAuditResult("x", calls2)
	if err != nil || len(fs) != 1 || fs[0].Title != "末次" {
		t.Fatalf("last submit_findings must win, got %d", len(fs))
	}
}

func TestLastToolCallArgs(t *testing.T) {
	calls := []ToolCall{{Name: "read", Arguments: "a"}, {Name: SubmitPatchesTool, Arguments: "p1"}, {Name: SubmitPatchesTool, Arguments: "p2"}}
	if v, ok := LastToolCallArgs(calls, SubmitPatchesTool); !ok || v != "p2" {
		t.Fatalf("want p2 got %q ok=%v", v, ok)
	}
	if _, ok := LastToolCallArgs(calls, "absent"); ok {
		t.Fatal("absent tool must be ok=false")
	}
}

// 空列表陷阱回归锁（gw-5a96f1f7 实证修复）：submit_findings 提交 {"findings":[]} 是
// 合法产出（完整审计后零发现）。判据须是"至少一批参数解析成功"而非 len(merged)>0——
// 后者把干净零发现误判为两代通道皆空 → "no JSON in DSH output" 整轮报废降级 RuleScan。
func TestParseAuditResult_EmptyFindingsViaToolIsValid(t *testing.T) {
	prose := "对 /sandbox/project 全量源码审计完成，未发现可利用的安全漏洞，已按输出契约提交空 findings 列表。"
	fs, ch, err := parseAuditResult(prose, []ToolCall{{Name: SubmitFindingsTool, Arguments: `{"findings": []}`}})
	if err != nil || ch != SubmitFindingsTool || len(fs) != 0 {
		t.Fatalf("empty submission is a valid zero-finding result: ch=%s err=%v n=%d", ch, err, len(fs))
	}
	// 混合批次：一批损坏 + 一批空 → 仍是有效零发现（anyParsed 来自空批）
	fs, ch, err = parseAuditResult(prose, []ToolCall{
		{Name: SubmitFindingsTool, Arguments: "{broken"},
		{Name: SubmitFindingsTool, Arguments: `{"findings": []}`},
	})
	if err != nil || ch != SubmitFindingsTool || len(fs) != 0 {
		t.Fatalf("damaged+empty batches must be valid zero: ch=%s err=%v n=%d", ch, err, len(fs))
	}
	// 对照：未调用工具且正文无 JSON → 仍然失败（真实未提交不被空结果掩盖）
	if _, _, err := parseAuditResult(prose, nil); err == nil {
		t.Fatal("no tool call + prose-only final must still fail")
	}
}

func TestParseFindingsTrailingCommaRepair(t *testing.T) {
	// LLM 常见小错：尾逗号——修复层兜住（截断仍拒绝，走失败反馈再生成）
	bad := "```json\n{\"findings\": [{\"title\": \"t\", \"file_path\": \"f\",},],}\n```"
	fs, err := ParseFindings(bad)
	if err != nil || len(fs) != 1 || fs[0].Title != "t" {
		t.Fatalf("trailing commas must be repaired: err=%v n=%d", err, len(fs))
	}
	// 截断 JSON 不可修复
	if _, err := ParseFindings("```json\n{\"findings\": [{\"title\": \"unclosed\n```"); err == nil {
		t.Fatal("truncated JSON must fail (honest failure → regeneration)")
	}
}

// ADR-192 回归锁：主会话推理流瞬态中断（STREAM_CLOSED——gw-d911757 实证：7 项发现
// 确认完毕、死于 submit_findings 参数流式生成途中）→ 继续指令重试后模型重发提交。
func TestRun_MainTurnTransientRetry(t *testing.T) {
	const submitArgs = `{"findings":[{"title":"stub","severity":"SEVERITY_HIGH","cwe_id":"CWE-89","file_path":"a.py","start_line":1,"confidence":0.9,"description":"d","reasoning":"r"}]}`
	seg1 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"All analysis is complete. Submitting the findings."}]}}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":30,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"SSE stream ended without [DONE]","code":"STREAM_CLOSED"}}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	seg2 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":40,\"data\":{\"turn\":2}}}\n\n",
		`event: session.event` + "\n" + `data: {"sessionId":"main","event":{"type":"tool/call","seq":50,"data":{"turn":2,"step":1,"callId":"c-1","name":"submit_findings","arguments":"` + jsEscape(submitArgs) + `"}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":70,\"data\":{\"turn\":2,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{scripts: [][]string{seg1, seg2}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	var events logwriter
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-transient", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v（修复前此处即回合失败）", err)
	}
	if !res.OK || len(res.Findings) != 1 || res.Findings[0].Title != "stub" {
		t.Fatalf("result: %+v", res)
	}
	fb.mu.Lock()
	n, last := fb.promptCount, fb.promptText
	fb.mu.Unlock()
	if n != 2 || !strings.Contains(last, "截断") {
		t.Fatalf("prompts: n=%d last=%.80s（应为初始+继续指令）", n, last)
	}
	if !strings.Contains(events.String(), "ADR-192") {
		t.Fatalf("retry event log missing:\n%s", events.String())
	}
}

// ADR-192：非瞬态错误（参数非法等）不重试，立即失败。
func TestRun_MainTurnPermanentErrorFails(t *testing.T) {
	fb := &fakeBridge{script: []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":20,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"InvalidRequest: max_tokens invalid","code":"INVALID_REQUEST"}}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	})
	_, err := r.Run(context.Background(), Task{
		TaskID: "t-perm", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "InvalidRequest") {
		t.Fatalf("permanent error must fail fast: %v", err)
	}
	fb.mu.Lock()
	n := fb.promptCount
	fb.mu.Unlock()
	if n != 1 {
		t.Fatalf("prompt count = %d, want 1 (no retry on permanent error)", n)
	}
}

// ADR-192：瞬态重试耗尽（≤2 轮）后如实失败，绝不无限循环。
func TestRun_MainTurnRetryExhausted(t *testing.T) {
	errFrame := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":20,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"SSE stream ended without [DONE]","code":"STREAM_CLOSED"}}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{scripts: [][]string{errFrame, errFrame, errFrame}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	})
	_, err := r.Run(context.Background(), Task{
		TaskID: "t-exhaust", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "SSE stream ended") {
		t.Fatalf("exhausted retries must fail: %v", err)
	}
	fb.mu.Lock()
	n := fb.promptCount
	fb.mu.Unlock()
	if n != 3 { // 初始 + 2 次重试
		t.Fatalf("prompt count = %d, want 3 (initial + 2 retries)", n)
	}
}

// ADR-194 回归锁：分批提交跨断流重试合并——批1（2 条）提交成功后断流，
// 继续指令驱动回合 2 补齐批2（2 条），Run 收齐 4 条（已提交批次不因新回合丢失，
// 模型重发批1 由去重吸收）。
func TestRun_BatchedSubmitMergeAcrossRetry(t *testing.T) {
	mkArgs := func(ids ...string) string {
		fs := make([]string, 0, len(ids))
		for _, id := range ids {
			fs = append(fs, `{"title":"`+id+`","severity":"SEVERITY_HIGH","cwe_id":"CWE-89","file_path":"a.py","start_line":1,"confidence":0.9,"description":"d","reasoning":"r"}`)
		}
		return `{"findings":[` + strings.Join(fs, ",") + `]}`
	}
	submitCall := func(seq, args string) string {
		return "event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"tool/call","seq":` + seq +
			`,"data":{"turn":1,"step":1,"callId":"c-` + seq + `","name":"submit_findings","arguments":"` + jsEscape(args) + `"}}}` + "\n\n"
	}
	completedTurnEnd := func(seq, turn string) string {
		return "event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":` + seq +
			`,"data":{"turn":` + turn + `,"reason":{"kind":"completed"}}}}` + "\n\n"
	}
	streamBreakTurnEnd := func(seq, turn string) string {
		return "event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":` + seq +
			`,"data":{"turn":` + turn + `,"reason":{"kind":"error","error":{"message":"SSE stream ended without [DONE]","code":"STREAM_CLOSED"}}}}}` + "\n\n"
	}
	seg1 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		submitCall("11", mkArgs("f1", "f2")), // 批1 落袋
		streamBreakTurnEnd("20", "1"),
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	seg2 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":40,\"data\":{\"turn\":2}}}\n\n",
		submitCall("41", mkArgs("f3", "f4")), // 批2 补齐
		submitCall("42", mkArgs("f1", "f2")), // 模型重发批1：去重吸收
		completedTurnEnd("70", "2"),
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{scripts: [][]string{seg1, seg2}}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.1",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 32768,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-batch", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := map[string]bool{}
	for _, f := range res.Findings {
		got[f.Title] = true
	}
	for _, want := range []string{"f1", "f2", "f3", "f4"} {
		if !got[want] {
			t.Fatalf("batched submit must merge across retry: missing %q, got %v", want, got)
		}
	}
	if len(res.Findings) != 4 {
		t.Fatalf("dedup must absorb re-sent batch: got %d findings", len(res.Findings))
	}
}
