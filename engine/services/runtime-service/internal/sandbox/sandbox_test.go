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
	frames      chan string // SSE 回放帧通道（subscribe handler 唯一消费）
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
		// re-review P3 根治：frames 经 channel、由本 handler 唯一持有 Writer 转发
		// （回放 goroutine 锁外直写 Writer 会与 handler 退出的 finishRequest 竞争
		// ——http.ResponseWriter 生命周期归 handler，-race 实测基线 flake）。
		f.mu.Lock()
		if f.frames == nil {
			f.frames = make(chan string, 64)
		}
		f.sub = w
		f.subCtx = r.Context()
		f.mu.Unlock()
		for {
			select {
			case <-r.Context().Done():
				return
			case frame := <-f.frames:
				fmt.Fprint(w, frame)
				flusher.Flush()
			}
		}
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
			for _, frame := range script {
				select {
				case f.frames <- frame:
				case <-time.After(5 * time.Second): // 无订阅者兜底（防 goroutine 泄漏）
					return
				}
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
	lastSpec     map[string]any // M1：最近一次创建请求的 spec（providers 挂载断言）
	deleted      []string
	execCount    int
	launchScript string
	bridgeURL    string
	failLaunch   bool
	uploadCount  int    // ADR-187：项目 tar.gz 上传（POST files）次数
	uploadPath   string // 最近一次上传的 path 字段
	// ADR-227：可选推理路由端点（llmRouteEnv 分派测试）。启动前设置、运行期只读。
	// 不设置时 /api/v1/inference/* 全 404 → llmRouteEnv 诚实降级回 deepseek env。
	routeJSON    string
	providerJSON map[string]string
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
		f.lastSpec, _ = body["spec"].(map[string]any)
		f.mu.Unlock()
		spec, _ := body["spec"].(map[string]any)
		// spec 不得携带 policy：spec.policy 会整体覆盖镜像 policy.yaml（替换语义），
		// 极简清单缺基路径令 supervisor 自举即毙（ContainerExited → error phase）。
		// 镜像 policy.yaml 是唯一权威。
		if _, has := spec["policy"]; has {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":"spec must not carry policy (image policy.yaml is authoritative)"}`)
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
	f.serveInference(mux)
	return mux
}

// serveInference — ADR-227 可选推理端点挂载（routeJSON/providerJSON 非空时注册）。
func (f *fakeManager) serveInference(mux *http.ServeMux) {
	if f.routeJSON != "" {
		mux.HandleFunc("/api/v1/inference/route", func(w http.ResponseWriter, r *http.Request) {
			if !f.auth(r) {
				w.WriteHeader(401)
				fmt.Fprintf(w, `{"error":"unauthorized"}`)
				return
			}
			fmt.Fprintf(w, "%s", f.routeJSON)
		})
	}
	if len(f.providerJSON) > 0 {
		mux.HandleFunc("/api/v1/inference/providers/", func(w http.ResponseWriter, r *http.Request) {
			if !f.auth(r) {
				w.WriteHeader(401)
				fmt.Fprintf(w, `{"error":"unauthorized"}`)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/inference/providers/")
			pj, ok := f.providerJSON[name]
			if !ok {
				w.WriteHeader(404)
				fmt.Fprintf(w, `{"error":"not found"}`)
				return
			}
			fmt.Fprintf(w, "%s", pj)
		})
	}
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.2",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		// 测试中沙箱服务域无 DNS：显式把服务路由拨号指到假 bridge（等价生产网关直拨）
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		OnHumanLog:      human.writeString,
		OnRawLog:        raw.write,
	}))
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

	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	}))
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	}))
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	}))
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

// R34 回归锁（gw-d331089f 实证）：补丁再生成回合（PatchFixRound）按契约分批调用
// submit_patches、正文无 JSON——Run 曾套用审计回合的 findings 语义解析，报
// "no JSON in DSH output" 判废整轮，模型合规提交的 4 批补丁全被丢弃（9 分钟
// 再生成沙箱白跑）。修复后 Run 按 patches 语义合并工具批次产出。
func TestRun_PatchFixRoundToolBatchesMerged(t *testing.T) {
	patchCall := func(seq, idx string) string {
		args := `{"patches":[{"index":` + idx + `,"diff_patch":"*** Begin Patch\n*** Update File: a.py\n@@ print('x')\n-print('x')\n+print('fixed-` + idx + `')\n*** End Patch"}]}`
		return "event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"tool/call","seq":` + seq +
			`,"data":{"turn":1,"step":1,"callId":"c-` + seq + `","name":"submit_patches","arguments":"` + jsEscape(args) + `"}}}` + "\n\n"
	}
	seg := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		patchCall("11", "0"), patchCall("12", "1"), patchCall("13", "2"), patchCall("14", "0" /*重发同 index：后批覆盖*/),
		// 最终消息：纯正文无任何 JSON（真实回合形态，gw-d331089f .ai.log 实证）
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"` + jsEscape("全部 13 项已重新生成并分批提交，根因是路径前缀。") + `"}]}}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":30,"data":{"turn":1,"reason":{"kind":"completed"}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{script: seg}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.1",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 32768,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	}))
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-fix", WorkspaceDir: newTestWorkspace(t), Assignment: "重出补丁", Timeout: 10 * time.Second,
		PatchFixRound: true,
	})
	if err != nil {
		t.Fatalf("patch-fix round must parse via patches semantics (R34), got: %v", err)
	}
	if !res.OK {
		t.Fatal("res.OK must be true")
	}
	if len(res.Patches) != 3 {
		t.Fatalf("batches must merge by index with last-wins (0 duplicated): got %d", len(res.Patches))
	}
	if res.Patches[0].Index != 0 || !strings.Contains(res.Patches[0].DiffPatch, "fixed-0") {
		t.Fatalf("later batch must win for same index: %+v", res.Patches[0])
	}
	if res.Patches[1].Index != 1 || res.Patches[2].Index != 2 {
		t.Fatalf("indexes must be ascending: %+v", res.Patches)
	}
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
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
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
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 5,
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
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 1,
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
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 1,
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
	// 2026-09-17 两段式：一阶段纯检测——apply_patch 教程随补丁职责移入第二阶段
	// （patchPhaseTemplate），prompt 锚点相应更新
	for _, want := range []string{ProjectSandboxPath, "对项目做安全审计", "submit_findings"} {
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

// ADR-241 回归锁：一阶段纯检测契约——diff_patch 恒空、
// 单档 ≤16/批、打满上限措辞（A1，治模型自选保守批量）；apply_patch 补丁教程不得回流
// 一阶段（注意力分段满配）；治理引用（ADR-xxx/gw-xxx 考古）不得进 prompt（第一性
// 重设计 D1/D2：决策台账编号与事故故事对沙箱模型零语义）。
func TestBuildTurnPrompt_Phase1DetectionContract(t *testing.T) {
	prompt := buildTurnPrompt(Task{Assignment: "审计它"})
	for _, want := range []string{
		"第一阶段：检测",            // 两段式标题
		"diff_patch 一律给空字符串", // 检测/补丁分离的核心指令
		"补丁由第二阶段统一生成",        // 职责去向可溯
		"每批最多 16 条",          // 档位 8→16（人类裁决"太小"，2026-09-17）
		"打满上限、勿自行缩小",         // A1：治模型保守批量（实测自选 2/批白损吞吐）
		"没有发现时提交空数组",         // 零发现合法产出（gw-5a96f1f7 同源纪律）
		`"diff_patch": ""`,   // 兜底 schema 钉死空串（工具不可用降级路径唯一字段规范）
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("phase-1 detection contract missing %q:\n%s", want, prompt)
		}
	}
	for _, banned := range []string{
		"*** Begin Patch", // 补丁教程只属第二阶段
		"submit_patches",  // 一阶段只用 submit_findings
		"ADR-",            // 治理引用不得进沙箱 prompt
		"gw-",             // 事故考古不得进沙箱 prompt
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("phase-1 prompt must not contain %q:\n%.400s", banned, prompt)
		}
	}
}

// ADR-241 回归锁：第二阶段（补丁生成）prompt——apply_patch 格式五条 + 清单注入 +
// 档位 ≤8/批 + 反编造。
func TestBuildPatchPhasePrompt(t *testing.T) {
	manifest := `[{"index":0,"title":"SQL 注入","file_path":"a.py","start_line":6,"fix_suggestion":"参数化查询"}]`
	prompt := BuildPatchPhasePrompt(manifest)
	for _, want := range []string{
		"第二阶段",             // 标题
		ProjectSandboxPath, // 沙箱路径占位
		"逐项生成修复补丁",         // 任务指令
		"上下文/删除行必须与源码文件逐字一致", // 反编造（逐字复制）
		"不要凭记忆改写",            // 反编造（第二处，双保险收敛后仅存的两处之一）
		"*** Begin Patch",    // 格式规范
		"*** End of File",    // 魔法语义细节
		"NFC",                // 归一化要求
		manifest,             // 清单 JSON 原文注入
		"submit_patches",     // 提交工具
		"每批最多 8 条",           // 档位 4→8（人类裁决"太小"，2026-09-17）
		"单独成批",               // 超大补丁单条层
		"不要编造",               // A3 反编造
		`{"patches"`,         // 兜底 schema
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("phase-2 patch prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "reasoning") {
		t.Fatal("phase-2 manifest must not carry reasoning (position+intent only)")
	}
}

// 批次观测埋点（ADR-241 观测两指标之一：单批条数打满度）——解析正确性锁定。
func TestBatchSizes(t *testing.T) {
	calls := []ToolCall{
		{Name: SubmitFindingsTool, Arguments: `{"findings":[{}, {},{}]}`},
		{Name: "other_tool", Arguments: `{"findings":[{}]}`},
		{Name: SubmitFindingsTool, Arguments: `{"findings":[]}`},
		{Name: SubmitFindingsTool, Arguments: `not-json`},
		{Name: SubmitPatchesTool, Arguments: `{"patches":[{},{}]}`},
	}
	got := batchSizes(calls, SubmitFindingsTool)
	if len(got) != 3 || got[0] != 3 || got[1] != 0 || got[2] != -1 {
		t.Fatalf("findings batch sizes = %v, want [3 0 -1]", got)
	}
	if got := batchSizes(calls, SubmitPatchesTool); len(got) != 1 || got[0] != 2 {
		t.Fatalf("patches batch sizes = %v, want [2]", got)
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
		WaitReadyTimeoutS: 120, DSHMaxTokens: 131072,
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

// R34：补丁再生成回合结果源选择——工具参数优先分批合并（同 index 后批覆盖），
// 围栏降级，空 patches 合法（与 parseAuditResult 零发现口径同源）。
func TestParsePatchFixResult(t *testing.T) {
	batch := func(idxs ...int) string {
		items := make([]string, 0, len(idxs))
		for _, i := range idxs {
			items = append(items, `{"index":`+fmt.Sprintf("%d", i)+`,"diff_patch":"*** Begin Patch\n*** Update File: a.py\n@@ x\n-x\n+y`+fmt.Sprintf("%d", i)+`\n*** End Patch"}`)
		}
		return `{"patches":[` + strings.Join(items, ",") + `]}`
	}
	calls := []ToolCall{
		{Name: SubmitPatchesTool, Arguments: batch(0, 1)},
		{Name: SubmitPatchesTool, Arguments: batch(2)},
	}
	ps, ch, err := parsePatchFixResult("正文无关", calls)
	if err != nil || ch != SubmitPatchesTool || len(ps) != 3 || ps[0].Index != 0 || ps[2].Index != 2 {
		t.Fatalf("tool batches must merge ascending: ch=%s n=%d err=%v", ch, len(ps), err)
	}
	// 同 index 后批覆盖（模型自纠重发以最新为准）
	calls2 := []ToolCall{
		{Name: SubmitPatchesTool, Arguments: batch(0)},
		{Name: SubmitPatchesTool, Arguments: batch(0)},
	}
	ps, _, err = parsePatchFixResult("x", calls2)
	if err != nil || len(ps) != 1 {
		t.Fatalf("same index must dedup: n=%d err=%v", len(ps), err)
	}
	// 工具损坏 → 围栏降级
	fence := "```json\n{\"patches\":[{\"index\":5,\"diff_patch\":\"p\"}]}\n```"
	ps, ch, err = parsePatchFixResult(fence, []ToolCall{{Name: SubmitPatchesTool, Arguments: "{broken}"}})
	if err != nil || ch != "text-fence" || len(ps) != 1 || ps[0].Index != 5 {
		t.Fatalf("fence fallback must engage: ch=%s err=%v", ch, err)
	}
	// 工具提交空列表=合法零产出（不落围栏）
	ps, ch, err = parsePatchFixResult("正文", []ToolCall{{Name: SubmitPatchesTool, Arguments: `{"patches":[]}`}})
	if err != nil || ch != SubmitPatchesTool || len(ps) != 0 {
		t.Fatalf("empty patches via tool is valid: ch=%s n=%d err=%v", ch, len(ps), err)
	}
	// 两代通道皆空 → 报错
	if _, _, err := parsePatchFixResult("无 JSON", nil); err == nil {
		t.Fatal("no channel must error")
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	}))
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
	// ADR-211：续跑指令必须带自适应缩批要求——原批量重试=重蹈覆辙（同一断流热点再撞一次）
	if n != 2 || !strings.Contains(last, "截断") || !strings.Contains(last, "每批 1 条") {
		t.Fatalf("prompts: n=%d last=%.80s（应为初始+带缩批要求的继续指令）", n, last)
	}
	if !strings.Contains(events.String(), "ADR-192") {
		t.Fatalf("retry event log missing:\n%s", events.String())
	}
}

// ADR-227：流中途数据级损坏（malformed SSE payload）与连接级断流同族——会话历史
// 完好，续跑指令可恢复；不得一帧坏 JSON 即判回合死刑拆沙箱。
// 实证 gw-a00057d9691e0097f1b546da：流式推理 2m39s 后一帧 JSON.parse 失败，
// 非瞬态判定→整回合作废→沙箱回收→RuleScan 降级。
func TestRun_MainTurnMalformedPayloadRetry(t *testing.T) {
	const submitArgs = `{"findings":[{"title":"stub","severity":"SEVERITY_HIGH","cwe_id":"CWE-89","file_path":"a.py","start_line":1,"confidence":0.9,"description":"d","reasoning":"r"}]}`
	seg1 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":20,"data":{"message":{"content":[{"type":"text","text":"All analysis is complete. Submitting the findings."}]}}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":30,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"malformed SSE payload: {\"id\":\"202609121435093265b73e630146a2\",\"object\":\"chat.completion.chunk\"","code":"MALFORMED_RESPONSE"}}}}}` + "\n\n",
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		EventFn:         func(level, msg string) { events.write(fmt.Sprintf("[%s] %s\n", level, msg)) },
	}))
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-malformed", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它", Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v（修复前畸形帧即回合失败、沙箱回收）", err)
	}
	if !res.OK || len(res.Findings) != 1 || res.Findings[0].Title != "stub" {
		t.Fatalf("result: %+v", res)
	}
	fb.mu.Lock()
	n, last := fb.promptCount, fb.promptText
	fb.mu.Unlock()
	if n != 2 || !strings.Contains(last, "截断") || !strings.Contains(last, "每批 1 条") {
		t.Fatalf("prompts: n=%d last=%.80s（应为初始+带缩批要求的继续指令）", n, last)
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	}))
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.0",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	}))
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
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.1",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 32768,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
	}))
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

// ADR-212 回归：launch 在创建动作前注册 activeSandboxes（防对账竞态），创建
// 失败必须注销——否则每次失败泄一条注册表记录；最坏情形（manager 实际已建但
// 响应丢失）真孤儿沙箱被本进程注册表永久屏蔽，ADR-210 对账器永不回收。
func TestRun_CreateFailure_DeregistersActiveEntry(t *testing.T) {
	countActive := func() int {
		n := 0
		activeSandboxes.Range(func(_, _ any) bool { n++; return true })
		return n
	}
	before := countActive()

	fm := &fakeManager{token: "secret"} // runner 凭据错 → 创建 401 失败
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "wrong",
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 1,
	})
	if _, err := r.Run(context.Background(), Task{TaskID: "t", WorkspaceDir: newTestWorkspace(t), Assignment: "x"}); err == nil {
		t.Fatal("create failure must surface")
	}
	if after := countActive(); after != before {
		t.Fatalf("create failure leaked registry entry: before=%d after=%d", before, after)
	}
}

// feedAssistantAttempt — assistant 正文契约两测试共用的帧注入：stream 记录串经
// json.Unmarshal+Marshal 构造 wire——手写转义层数错误会被 consume 静默吞帧
// （曾致 R92 测试假红，见 8d16613d）。
func feedAssistantAttempt(t *testing.T, p *sseParser, records string) {
	t.Helper()
	var parsed []any
	if err := json.Unmarshal([]byte(records), &parsed); err != nil {
		t.Fatalf("test fixture records must be valid JSON: %v", err)
	}
	wire, err := json.Marshal(map[string]any{"sessionId": "main", "event": map[string]any{
		"type": "assistant/attempt", "data": map[string]any{"turn": 1, "step": 2, "stream": parsed},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p.line([]byte("event: session.event\n"))
	p.line([]byte("data: " + string(wire) + "\n"))
	p.line([]byte("\n"))
}

// TestSSEParser_AssistantAttempt — dsh 0.1.5 正文契约回归锁（R86）：
// assistant 正文从逐 chunk 事件（0.1.2）改为步骤收敛时一次性 assistant/attempt
// （data={turn,step,stream}，记录形态=上游 expandAssistantStream 输入）。
// 渲染必须与 0.1.2 流式路径等价：💭/✍ 分块头+正文原样+块尾换行；
// tool-call-chunks（工具参数流）与 usage 静默（ADR-181）；
// 空正文不出头；assistant/message 仍只作收敛投影不重复输出。
func TestSSEParser_AssistantAttempt(t *testing.T) {
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
	feedAssistantAttempt(t, p, `[{"type":"reasoning-chunks","index":0,"time0":1,"dt":[1],"texts":["分析 ","app.py"]},{"type":"text-chunks","index":1,"time0":3,"dt":[1],"texts":["发现 1 处 SQL 注入"]}]`)
	if !strings.Contains(human.String(), "💭 [思考]\n分析 app.py") {
		t.Fatalf("reasoning block must render with head and joined deltas, got %q", human.String())
	}
	if !strings.Contains(human.String(), "✍ [输出]\n发现 1 处 SQL 注入") {
		t.Fatalf("text block must render with head and joined deltas, got %q", human.String())
	}
	// 单 chunk 记录形态等价（上游 expandAssistantStream 对 chunk 记录原样透传）
	feedAssistantAttempt(t, p, `[{"type":"chunk","time":9,"chunk":{"type":"text-delta","index":1,"text":"补充说明"}}]`)
	if !strings.Contains(human.String(), "✍ [输出]\n补充说明") {
		t.Fatalf("single chunk record must render like text-chunks, got %q", human.String())
	}
	// tool-call-chunks 静默（ADR-181）；空 texts 不出头。args 曾误写多一 } 成非法
	// JSON，被 consume 静默吞帧致静默断言空转假绿——Marshal 通道后显形。
	before := len(human.String())
	feedAssistantAttempt(t, p, `[{"type":"tool-call-chunks","index":2,"time0":5,"dt":[1],"args":["{\"cmd\":","x\"}"]}]`)
	feedAssistantAttempt(t, p, `[{"type":"text-chunks","index":3,"time0":7,"dt":[],"texts":[]}]`)
	if after := human.String(); len(after) != before {
		t.Fatalf("tool-call-chunks and empty texts must be silent, got %q", after[before:])
	}
	// assistant/message（0.1.5 帧形，自带 data.stream）：正文唯一载体——必须渲染
	// 且同时投 assistantText（turn() 收敛判据）。
	evs := feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":1,\"step\":3,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"最终摘要\"}]},\"stream\":[{\"type\":\"text-chunks\",\"index\":4,\"time0\":8,\"dt\":[1],\"texts\":[\"最终摘要\"]}]}}}\n", "\n")
	if len(evs) == 0 || evs[0].assistantText != "最终摘要" {
		t.Fatalf("assistant/message must project assistantText, got %+v", evs)
	}
	if !strings.Contains(human.String(), "✍ [输出]\n最终摘要") {
		t.Fatalf("assistant/message with stream must render body, got %q", human.String())
	}
	// assistant/message（0.1.2 帧形，无 stream）：正文已由 chunk 流式呈现——静默防双渲染
	n := len(human.String())
	if evs := feed("event: session.event\n",
		"data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"012时代的重复正文\"}]}}}}\n", "\n"); len(evs) == 0 {
		t.Fatal("assistant/message must still project assistantText for settlement")
	}
	if after := human.String(); strings.Contains(after[n:], "012时代的重复正文") {
		t.Fatalf("assistant/message without stream must not re-render body, got %q", after)
	}
}

// TestSSEParser_AssistantAttemptBlockBoundaries — R92 补强（审计 P3-3/P3-4）：
// ①块身份=(记录类型,index)：同类型不同 index 的两个 text 块不得粘连——index 变化
//
//	须补块尾换行+新头（对齐 0.1.2 的 block-end/block-start 语义）；
//
// ②tool-call-chunks 关闭当前正文块（0.1.2 中 tool 块的 block-start 会终结前块）；
// ③精确等值锁：头/换行/块尾逐字节锁定（Contains 断言锁不住边界换行回潮）；
// ④混合到达：0.1.2 chunk 路径的 p.mode 残态不得影响 0.1.5 message 路径（局部 mode 隔离）。
func TestSSEParser_AssistantAttemptBlockBoundaries(t *testing.T) {
	var human syncwriter
	p := &sseParser{onHuman: human.writeString}
	// ①+②：多 index 交替流——精确等值（先红：旧实现第二块粘连且无头、tool 块不闭前块）
	feedAssistantAttempt(t, p, `[{"type":"reasoning-chunks","index":0,"time0":1,"dt":[1],"texts":["推演"]},`+
		`{"type":"text-chunks","index":1,"time0":2,"dt":[1,2],"texts":["第一块","续"]},`+
		`{"type":"tool-call-chunks","index":2,"time0":5,"dt":[1],"args":["{\"c\":"]},`+
		`{"type":"text-chunks","index":3,"time0":7,"dt":[1],"texts":["第二块"]}]`)
	want := "\n💭 [思考]\n推演\n" +
		"\n✍ [输出]\n第一块续\n" +
		"\n✍ [输出]\n第二块\n"
	if got := human.String(); got != want {
		t.Fatalf("block boundaries mismatch\n got=%q\nwant=%q", got, want)
	}
	// ④混合到达：0.1.2 chunk 路径留下未闭合的 p.mode 残态（block-start 无 block-end），
	// 随后的 0.1.5 message 路径必须不受影响（局部 mode 隔离——头照发、边界正确）。
	p.line([]byte("event: session.event\n"))
	p.line([]byte("data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"block-start\",\"blockType\":\"text\"}}}}\n"))
	p.line([]byte("\n"))
	p.line([]byte("event: session.event\n"))
	p.line([]byte("data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"text-delta\",\"text\":\"旧路径残文\"}}}}\n"))
	p.line([]byte("\n"))
	n := len(human.String())
	feedAssistantAttempt(t, p, `[{"type":"text-chunks","index":4,"time0":9,"dt":[1],"texts":["新路径正文"]}]`)
	mixed := human.String()[n:]
	if mixed != "\n✍ [输出]\n新路径正文\n" {
		t.Fatalf("message path must be isolated from chunk-path p.mode residue\n got=%q", mixed)
	}
	if !strings.Contains(human.String(), "旧路径残文") {
		t.Fatal("chunk-path body must remain rendered")
	}
}

// feedStreamFrame — session.stream 逐字流帧注入（R93）：frame 经 json.Marshal
// 构造 wire（沿 feedAssistantAttempt 的 Marshal 通道纪律，杜绝手写转义被
// consume 静默吞帧致假红/假绿）。
func feedStreamFrame(t *testing.T, p *sseParser, sessionId string, frame map[string]any) {
	t.Helper()
	wire, err := json.Marshal(map[string]any{"sessionId": sessionId, "frame": frame})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p.line([]byte("event: session.stream\n"))
	p.line([]byte("data: " + string(wire) + "\n"))
	p.line([]byte("\n"))
}

// streamChunkFrame — 构造一条 frame.type=chunk 的 session.stream 帧。
// 字段对齐真实 AssistantStreamFrame（上游 runtime-types.ts:128-161）：chunk 帧
// 只有 attemptId/revision/index/time/chunk——turn/step 仅 start 帧携带（R93 补强：
// 初版夹具误在 chunk 帧造 turn/step，单测绿但真线上去重键恒 0 致 e2e 双显，
// sim gw-70829273 实证——夹具必须按线上真形状造）。
func streamChunkFrame(index int, chunk map[string]any) map[string]any {
	return map[string]any{
		"type": "chunk", "attemptId": "main:1", "revision": 2, "index": index,
		"time": 1758000000000, "chunk": chunk,
	}
}

// streamStartFrame — 构造 start 帧（attemptId → (turn,step) 登记的唯一来源）。
func streamStartFrame(attemptID string, turn, step float64) map[string]any {
	return map[string]any{"type": "start", "attemptId": attemptID, "revision": 1, "turn": turn, "step": step}
}

// TestSSEParser_SessionStream — R93 逐字流回归锁（ADR-236）：
// dsh-sdk-stream 插件（dsh-pentest-sse §I7/RG-012）把进程内
// agent/assistant-stream 帧作为 session.stream 通知透传——本渲染器负责：
// ①主会话 chunk 逐字实时渲染（💭/✍ 头+增量正文，语义=0.1.2 assistant/chunk）；
// ②(turn,step) 去重：本步已 live 渲染时，收敛 assistant/message 只投影
//
//	assistantText、不再展开内嵌 stream（防双渲染）；无 live 帧时回落 R92 展开；
//
// ③子会话流静默（ADR-181 同口径）；④start/end 帧不渲染。
func TestSSEParser_SessionStream(t *testing.T) {
	var human syncwriter
	p := &sseParser{onHuman: human.writeString}

	// start 帧不渲染
	feedStreamFrame(t, p, "main", map[string]any{
		"type": "start", "attemptId": "main:1", "revision": 1, "turn": float64(1), "step": float64(2),
	})
	if human.String() != "" {
		t.Fatalf("start frame must render nothing, got %q", human.String())
	}

	// ①主会话逐字渲染：reasoning 块 + text 块（block-start/δ/block-end 全链）
	feedStreamFrame(t, p, "main", streamChunkFrame(0, map[string]any{"type": "block-start", "index": 0, "blockType": "reasoning"}))
	feedStreamFrame(t, p, "main", streamChunkFrame(1, map[string]any{"type": "reasoning-delta", "index": 0, "text": "分析"}))
	feedStreamFrame(t, p, "main", streamChunkFrame(2, map[string]any{"type": "block-end", "index": 0, "block": map[string]any{"type": "reasoning", "text": "分析"}}))
	feedStreamFrame(t, p, "main", streamChunkFrame(3, map[string]any{"type": "block-start", "index": 1, "blockType": "text"}))
	feedStreamFrame(t, p, "main", streamChunkFrame(4, map[string]any{"type": "text-delta", "index": 1, "text": "发现注入"}))
	feedStreamFrame(t, p, "main", streamChunkFrame(5, map[string]any{"type": "block-end", "index": 1, "block": map[string]any{"type": "text", "text": "发现注入"}}))
	if got, want := human.String(), "\n💭 [思考]\n分析\n"+"\n✍ [输出]\n发现注入\n"; got != want {
		t.Fatalf("live deltas must render verbatim with block heads\n got=%q\nwant=%q", got, want)
	}

	// ②(1,2) 已 live 渲染 → 收敛 message 不再展开内嵌 stream，但 assistantText 投影仍在
	var evs []bridgeEvent
	wire, _ := json.Marshal(map[string]any{"sessionId": "main", "event": map[string]any{
		"type": "assistant/message",
		"data": map[string]any{"turn": float64(1), "step": float64(2),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "发现注入"}}},
			"stream":  []any{map[string]any{"type": "text-chunks", "index": 1, "time0": 5, "dt": []any{1}, "texts": []any{"发现注入"}}},
		},
	}})
	p.line([]byte("event: session.event\n"))
	if ev := p.line([]byte("data: " + string(wire) + "\n")); ev != nil {
		evs = append(evs, *ev)
	}
	p.line([]byte("\n"))
	if len(evs) == 0 || evs[0].assistantText != "发现注入" {
		t.Fatalf("converged message must still project assistantText, got %+v", evs)
	}
	if got := human.String(); strings.Count(got, "发现注入") != 1 {
		t.Fatalf("live-rendered step must not re-render body via embedded stream (dedup), got %q", got)
	}

	// ③无 live 帧的 message（不同 step）→ R92 展开路径不受影响（回归保护）
	p.line([]byte("event: session.event\n"))
	if ev := p.line([]byte("data: {\"sessionId\":\"main\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":1,\"step\":3,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"批量兜底正文\"}]},\"stream\":[{\"type\":\"text-chunks\",\"index\":0,\"time0\":8,\"dt\":[1],\"texts\":[\"批量兜底正文\"]}]}}}\n")); ev != nil {
		evs = append(evs, *ev)
	}
	p.line([]byte("\n"))
	if !strings.Contains(human.String(), "✍ [输出]\n批量兜底正文") {
		t.Fatalf("message without prior live frames must render via R92 expansion, got %q", human.String())
	}

	// ④子会话流静默 + end 帧不渲染
	n := len(human.String())
	feedStreamFrame(t, p, "subagent-uuid", streamChunkFrame(0, map[string]any{"type": "text-delta", "index": 0, "text": "子任务正文不倾倒"}))
	feedStreamFrame(t, p, "main", map[string]any{
		"type": "end", "attemptId": "main:1", "revision": 3, "index": 6, // 真实 end 帧同样无 turn/step
		"outcome": map[string]any{"kind": "committed", "eventType": "assistant/message", "seq": float64(42)},
	})
	if after := human.String(); after != human.String()[:n] {
		t.Fatalf("subagent stream and end frame must be silent, got %q", after[n:])
	}
}

// ---- R97/R98 回归锁----
// 症状：补丁回合 prompt 被审计骨架嵌套（两套输出契约并存）→ 模型困惑推理/重复
// 提交 submit_findings/自发仿真验证；两回合人性化流都显示"第 1 轮开始"且无阶段标注。

// patchRoundFrames — 补丁回合最小帧序：turn/start(turn=1) → submit_patches → 收敛。
func patchRoundFrames(patchJSON, turn string) []string {
	ev := func(typ, data string) string {
		return "event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"` + typ + `","seq":` + turn + `0,"data":` + data + `}}` + "\n\n"
	}
	return []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		ev("turn/start", `{"turn":`+turn+`}`),
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"tool/call","seq":` + turn + `1,"data":{"turn":` + turn + `,"step":1,"callId":"c-p","name":"submit_patches","arguments":"` + jsEscape(patchJSON) + `"}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"assistant/message","seq":` + turn + `5,"data":{"message":{"content":[{"type":"text","text":"` + jsEscape("补丁已提交") + `"}]}}}}` + "\n\n",
		ev("turn/end", `{"turn":`+turn+`,"reason":{"kind":"completed"}}`),
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
}

// concludeOff — 机制类测试锁（假桥不建模提交通道，零提交是夹具形态而非缺陷）：
// 显式关 conclude 能力保持前收笔时代单回合形状；收笔行为由
// TestRun_MockDescriptor_Conclude* 专项锁（ADR-246）。
func concludeOff(c Config) Config {
	c.Agent = DefaultDescriptor()
	c.Agent.Capabilities.ConcludeSemantics = false
	return c
}

func newSandboxTestRunner(t *testing.T, fb *fakeBridge, human func(string), eventFn func(string, string)) (*ManagerRunner, *fakeBridge) {
	t.Helper()
	bridgeSrv := httptest.NewServer(fb.handler())
	t.Cleanup(bridgeSrv.Close)
	fm := &fakeManager{token: "tok-1", bridgeURL: bridgeSrv.URL + "/"}
	srv := httptest.NewServer(fm.handler())
	t.Cleanup(srv.Close)
	r := NewManagerRunner(concludeOff(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.1",
		WaitReadyTimeoutS: 5, DSHMaxTokens: 32768,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		OnHumanLog:      human, EventFn: eventFn,
	}))
	return r, fb
}

// R97 锁：补丁回合 prompt 必须是 assignment 逐字直发——曾无条件套审计骨架
// （phase1 模板嵌 phase2 全文），模型在思考里困惑挣扎数百 token 并重复提交 findings。
func TestRun_PatchFixRoundPromptIsAssignmentVerbatim(t *testing.T) {
	fb := &fakeBridge{script: patchRoundFrames(`{"patches":[{"index":0,"diff_patch":"*** Begin Patch\n*** End Patch"}]}`, "1")}
	r, fb := newSandboxTestRunner(t, fb, nil, nil)
	assignment := "# CodeAudit 补丁生成任务（第二阶段）\n\n发现清单（JSON）：\n[{\"index\":0,\"title\":\"x\"}]"
	if _, err := r.Run(context.Background(), Task{
		TaskID: "t-verbatim", WorkspaceDir: newTestWorkspace(t), Assignment: assignment,
		PatchFixRound: true, Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fb.promptText != assignment {
		t.Fatalf("patch-round prompt must be assignment verbatim:\n---got---\n%.300s\n---want---\n%.300s", fb.promptText, assignment)
	}
}

// R97 反向锁：审计回合 prompt 仍为 phase1 骨架嵌任务（不得矫枉过正丢模板）。
func TestRun_AuditTurnPromptWrapsAssignment(t *testing.T) {
	findingsJSON := `{"findings":[]}`
	fb := &fakeBridge{script: frames_Success(findingsJSON)}
	r, fb := newSandboxTestRunner(t, fb, nil, nil)
	if _, err := r.Run(context.Background(), Task{
		TaskID: "t-wrap", WorkspaceDir: newTestWorkspace(t), Assignment: "对项目做安全审计",
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"第一阶段：检测", "对项目做安全审计", "每批最多 16 条"} {
		if !strings.Contains(fb.promptText, want) {
			t.Fatalf("audit prompt missing %q:\n%.300s", want, fb.promptText)
		}
	}
}

// R98 锁：同一 runner 的两个回合（检测→补丁），人性化流轮次任务级连续（第二回合
// 显示"第 2 轮"），且补丁回合会话开始行带阶段标注。
func TestRun_TurnNumbersContinueAcrossPatchRound(t *testing.T) {
	var mu sync.Mutex
	var human []string
	collect := func(line string) {
		mu.Lock()
		human = append(human, line)
		mu.Unlock()
	}
	// 同一 runner 两回合（沙箱会话独立而 runner 复用——与编排层 applyPatchPhase 同构）；
	// fakeBridge 多段脚本：第 N 次 prompt 回放第 N 段。
	fb2 := &fakeBridge{scripts: [][]string{
		frames_Success(`{"findings":[]}`),
		patchRoundFrames(`{"patches":[]}`, "1"),
	}}
	r, _ := newSandboxTestRunner(t, fb2, collect, nil)
	if _, err := r.Run(context.Background(), Task{
		TaskID: "t-turns", WorkspaceDir: newTestWorkspace(t), Assignment: "审计", Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("audit run: %v", err)
	}
	if _, err := r.Run(context.Background(), Task{
		TaskID: "t-turns-fix", WorkspaceDir: newTestWorkspace(t),
		Assignment:    "# CodeAudit 补丁生成任务（第二阶段）",
		PatchFixRound: true, Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("patch run: %v", err)
	}
	mu.Lock()
	joined := strings.Join(human, "")
	mu.Unlock()
	if got := strings.Count(joined, "── 第 1 轮开始 ──"); got != 1 {
		t.Fatalf("turn-1 header count = %d, want 1 (第二回合必须连续化为第 2 轮):\n%s", got, joined)
	}
	if !strings.Contains(joined, "── 第 2 轮开始 ──") {
		t.Fatalf("second round must display as turn 2:\n%s", joined)
	}
	if !strings.Contains(joined, "══ DSH 会话开始（bridge）══ · 补丁生成") {
		t.Fatalf("patch round session line must carry phase tag:\n%s", joined)
	}
}

// R97 观测锁：补丁回合混入 submit_findings 调用必须 warn 上浮（按契约忽略但不可静默）。
func TestRun_PatchRoundUnexpectedFindingsCallWarns(t *testing.T) {
	var warns []string
	patch := `{"patches":[{"index":0,"diff_patch":"*** Begin Patch\n*** End Patch"}]}`
	fb := &fakeBridge{script: []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/start","seq":10,"data":{"turn":1}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"tool/call","seq":11,"data":{"turn":1,"step":1,"callId":"c-f","name":"submit_findings","arguments":"` + jsEscape(`{"findings":[]}`) + `"}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"tool/call","seq":12,"data":{"turn":1,"step":2,"callId":"c-p","name":"submit_patches","arguments":"` + jsEscape(patch) + `"}}}` + "\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":20,"data":{"turn":1,"reason":{"kind":"completed"}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}}
	r, _ := newSandboxTestRunner(t, fb, nil, func(level, msg string) {
		if level == "warn" {
			warns = append(warns, msg)
		}
	})
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-mixed", WorkspaceDir: newTestWorkspace(t), Assignment: "补丁", PatchFixRound: true,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Patches) != 1 {
		t.Fatalf("patches must still parse, got %d", len(res.Patches))
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "submit_findings") || !strings.Contains(joined, "忽略") {
		t.Fatalf("unexpected findings call must warn:\n%s", joined)
	}
}

// R97 对抗审查补遗（ADR-243）：补丁回合断流续跑指令必须按产出通道分岔——审计版
// 续跑指令让模型往 submit_findings 通道提交，混入调用被服务端按契约忽略=白烧 token。
// 消融验证：移除 patchRoundPrompt 分岔 → 本用例红（续跑 prompt 含 submit_findings）。
func TestRun_PatchRoundStreamBreakUsesPatchContinue(t *testing.T) {
	patchArgs := `{"patches":[{"index":0,"diff_patch":"*** Begin Patch\n*** End Patch"}]}`
	seg1 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":10,\"data\":{\"turn\":1}}}\n\n",
		"event: session.event\ndata: " + `{"sessionId":"main","event":{"type":"turn/end","seq":20,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"SSE stream ended without [DONE]","code":"STREAM_CLOSED"}}}}}` + "\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	seg2 := []string{
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"running\"}\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\",\"seq\":30,\"data\":{\"turn\":2}}}\n\n",
		`event: session.event` + "\n" + `data: {"sessionId":"main","event":{"type":"tool/call","seq":40,"data":{"turn":2,"step":1,"callId":"c-1","name":"submit_patches","arguments":"` + jsEscape(patchArgs) + `"}}}` + "\n\n",
		"event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"seq\":50,\"data\":{\"turn\":2,\"reason\":{\"kind\":\"completed\"}}}}\n\n",
		"event: session.status\ndata: {\"sessionId\":\"main\",\"status\":\"idle\"}\n\n",
	}
	fb := &fakeBridge{scripts: [][]string{seg1, seg2}}
	r, fb := newSandboxTestRunner(t, fb, nil, nil)
	res, err := r.Run(context.Background(), Task{
		TaskID: "t-patch-break", WorkspaceDir: newTestWorkspace(t), Assignment: "补丁任务",
		PatchFixRound: true, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK || len(res.Patches) != 1 {
		t.Fatalf("result: %+v", res)
	}
	fb.mu.Lock()
	n, last := fb.promptCount, fb.promptText
	fb.mu.Unlock()
	// 续跑指令必须指向补丁通道（submit_patches），不得复用审计版（submit_findings）
	if n != 2 || !strings.Contains(last, "截断") || !strings.Contains(last, "submit_patches") || strings.Contains(last, "submit_findings") {
		t.Fatalf("patch-round continue prompt must target patches channel: n=%d last=%.120s", n, last)
	}
}

// TestParseFindings_MultiFencePrefersFindings — 复批 II C5：正文先出现非 findings
// 围栏（模型引用配置内容）时择优取含 findings 键的围栏（gw-8bc1b9 实证首个围栏
// {"api_key":...} 抢占致 0 条）。
func TestParseFindings_MultiFencePrefersFindings(t *testing.T) {
	out := "审计中，读到配置：\n```json\n{\"api_key\": \"sk-live-xxx\"}\n```\n结论：\n```json\n{\"findings\":[{\"title\":\"SQL 注入\",\"file_path\":\"app.py\",\"start_line\":10,\"confidence\":0.9}]}\n```\n"
	fs, err := ParseFindings(out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(fs) != 1 || fs[0].Title != "SQL 注入" {
		t.Fatalf("应择优 findings 围栏得 1 条，got %d %+v", len(fs), fs)
	}
	// 单围栏（无 findings 键引用）语义不变：仍取该围栏（findings 缺省空数组）
	fs2, err2 := ParseFindings("```json\n{\"api_key\": \"k\"}\n```\n")
	if err2 != nil || len(fs2) != 0 {
		t.Fatalf("单围栏无 findings 键应得 0 条无错，got %d %v", len(fs2), err2)
	}
}
