// baton_hook 单测（TP17-T1）：参数拼装契约、降级不阻断反向（test-gates §3）、
// 真实二进制联跑（本机有 baton 兄弟仓时；CI fresh clone 无仓则 skip 并留说明）。
package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// stubBaton — 桩 CLI：记录 "$@" 到 argsFile；可选拷贝 marker 到 <out>/.tasklog/ 证活。
func stubBaton(t *testing.T, argsFile string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "baton-stub")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nfor a in \"$@\"; do case \"$a\" in -out) shift_count=1;; esac; done\n" +
		"out_prev=\nfor a in \"$@\"; do if [ \"$out_prev\" = \"1\" ]; then mkdir -p \"$a/.tasklog\"; printf 'stub-ok' > \"$a/.tasklog/events.jsonl\"; fi; case \"$a\" in -out) out_prev=1;; *) out_prev=;; esac; done\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeSSE(t *testing.T, dir, name string) string {
	t.Helper()
	sse := strings.Join([]string{
		"event: bridge.hello",
		`data: {"provider":"dsh","model":"m"}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"turn/start","data":{"turn":1}}}`,
		"",
	}, "\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAssignmentSummary(t *testing.T) {
	cases := []struct{ in, want string }{
		{"对项目做安全审计：识别漏洞。\n第二行不入摘要", "对项目做安全审计：识别漏洞。"},
		{"", ""},
		{"  首行带空白  \n次行", "首行带空白"},
	}
	for _, c := range cases {
		if got := assignmentSummary(c.in); got != c.want {
			t.Errorf("assignmentSummary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("长", 200)
	if got := assignmentSummary(long); len([]rune(got)) != batonGoalMaxRunes {
		t.Errorf("超长摘要应截 %d rune，got %d", batonGoalMaxRunes, len([]rune(got)))
	}
}

func TestBatonProjectTurn_Args(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	t.Setenv("BATON_BIN", stubBaton(t, argsFile))
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	ssePath := writeSSE(t, dir, "T-9001.sse.log")

	batonProjectTurn(batonPen{TaskID: "T-9001", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "对项目做安全审计：识别漏洞。\n更多细节")

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal("桩未被调用——投影链路未触达")
	}
	got := strings.Fields(string(raw))
	want := []string{"project", "-sse", ssePath, "-out", filepath.Join(dir, "tasklog", "T-9001"), "-task", "T-9001", "-goal", "对项目做安全审计：识别漏洞。", "-agent", "dsh"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("CLI 参数不符\n got: %v\nwant: %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "tasklog", "T-9001", ".tasklog", "events.jsonl")); err != nil {
		t.Errorf("桩产物未落位: %v", err)
	}
}

// 反向（test-gates §3）：钩子禁用（BATON_BIN 未配置/不存在）与无输入（.sse.log
// 缺失/空 goal）时必须静默早退——留存通道不得阻断分析主链路（ADR-237）。
func TestBatonProjectTurn_DisabledAndDegraded(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	called := func() bool { _, err := os.ReadFile(argsFile); return err == nil }

	// 未配置 BATON_BIN（yaml 无键+env 未设）：禁用态
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	ssePath := writeSSE(t, dir, "T-9002.sse.log")
	batonProjectTurn(batonPen{TaskID: "T-9002", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "goal")
	if called() {
		t.Error("未配置 BATON_BIN 时不得调用 CLI")
	}

	// BATON_BIN 指向不存在路径：诚实降级 log，不 panic 不调用
	t.Setenv("BATON_BIN", filepath.Join(dir, "no-such-bin"))
	batonProjectTurn(batonPen{TaskID: "T-9002", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "goal")
	if called() {
		t.Error("不存在的 BATON_BIN 不得调用 CLI")
	}

	// 无输入：.sse.log 文件不存在
	t.Setenv("BATON_BIN", stubBaton(t, argsFile))
	batonProjectTurn(batonPen{TaskID: "T-9002", Entry: &aiLogEntry{rawPath: filepath.Join(dir, "absent.sse.log")}, Origin: nil, Agent: ""}, "goal")
	if called() {
		t.Error(".sse.log 缺失时不得调用 CLI")
	}

	// 空 goal（任务卡为空）：无脊柱目标不投影
	batonProjectTurn(batonPen{TaskID: "T-9002", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "  \n ")
	if called() {
		t.Error("空 goal 时不得调用 CLI")
	}

	// 落盘未配置（rawPath 空）：早退
	batonProjectTurn(batonPen{TaskID: "T-9002", Entry: &aiLogEntry{}, Origin: nil, Agent: ""}, "goal")
	if called() {
		t.Error("rawPath 空时不得调用 CLI")
	}
}

// TestBatonProjectTurn_RealBinary — 真实 baton 二进制联跑（宪法 §2b ④：外部依赖
// 完成证据=真实调用；本机主仓库布局 engine/../baton 存在时执行，CI fresh clone skip）。
func TestBatonProjectTurn_RealBinary(t *testing.T) {
	repoRoot := "../../../.."
	batonRepo := filepath.Join(repoRoot, "..", "baton")
	if _, err := os.Stat(filepath.Join(batonRepo, "go.mod")); err != nil {
		t.Skipf("无 baton 兄弟仓（%s）——CI 布局，真实联跑证据由本机 evidence 留档", batonRepo)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("本机无 go 工具链")
	}
	bin := filepath.Join(t.TempDir(), "baton")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/baton")
	build.Dir = batonRepo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 baton 失败: %v\n%s", err, out)
	}

	dir := t.TempDir()
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	ssePath := writeSSE(t, dir, "T-9003.sse.log")

	batonProjectTurn(batonPen{TaskID: "T-9003", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "对项目做安全审计")

	evPath := filepath.Join(dir, "tasklog", "T-9003", ".tasklog", "events.jsonl")
	raw, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatalf("真实投影未产出 events.jsonl: %v", err)
	}
	if !strings.Contains(string(raw), `"task":"T-9003"`) || !strings.Contains(string(raw), "task.created") {
		t.Errorf("投影产物语义不符:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "tasklog", "T-9003", ".tasklog", "WORKSPACE.json")); err != nil {
		t.Errorf("WORKSPACE.json 未产出: %v", err)
	}
}

// ---- R2 冻结基线锚（TP17-T2）----

func TestBatonHandoffCheckpoint_NoArchiveSkips(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	t.Setenv("BATON_BIN", stubBaton(t, argsFile))
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	batonHandoffCheckpoint("T-9004", dir) // 无档案（投影未发生）
	if _, err := os.ReadFile(argsFile); err == nil {
		t.Error("无 .tasklog 档案时不得空锚（预约冻结/零回合语义）")
	}
}

func TestGitAnchorOf(t *testing.T) {
	if got := gitAnchorOf(""); got != "" {
		t.Errorf("空路径应返回空, got %q", got)
	}
	if got := gitAnchorOf(t.TempDir()); got != "" {
		t.Errorf("非 git 目录应返回空, got %q", got)
	}
}

// TestBatonFreezeHook_RealBinary — 真实 baton 端到端：冻结回调链
// （重投影 → checkpointed 锚 → 幂等重试跳过 → 增长流新基线）。本机有 baton
// 兄弟仓时执行；CI 布局 skip（证据由本机 evidence 留档）。
func TestBatonFreezeHook_RealBinary(t *testing.T) {
	repoRoot := "../../../.."
	batonRepo := filepath.Join(repoRoot, "..", "baton")
	if _, err := os.Stat(filepath.Join(batonRepo, "go.mod")); err != nil {
		t.Skipf("无 baton 兄弟仓（%s）——CI 布局", batonRepo)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("本机无 go 工具链")
	}
	bin := filepath.Join(t.TempDir(), "baton")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/baton")
	build.Dir = batonRepo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 baton 失败: %v\n%s", err, out)
	}

	dir := t.TempDir()
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	ssePath := writeSSE(t, dir, "T-9005.sse.log")

	// 回合收束投影（T1 路径）→ 冻结回调（R2：重投影+锚定）——经 sandbox gate
	// 真实触发（service 注册 → RunSession 回合边界同款路径）
	batonProjectTurn(batonPen{TaskID: "T-9005", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "对项目做安全审计")
	release := registerBatonFreeze(batonPen{TaskID: "T-9005", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "对项目做安全审计", dir)
	defer release()
	sandbox.PauseSession("T-9005")
	defer sandbox.ResumeSession("T-9005")
	go func() {
		time.Sleep(100 * time.Millisecond)
		sandbox.ResumeSession("T-9005")
	}()
	if err := sandbox.GateFor("T-9005").WaitFrozen(context.Background(), "T-9005"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "tasklog", "T-9005", ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("冻结后档案应含投影事件+checkpointed, got %d 行", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "handoff.checkpointed") {
		t.Errorf("末行应为 checkpointed:\n%s", lines[len(lines)-1])
	}

	// 幂等：冻结重试（SSE 无增长，重挂 gate 再冻结）不得重复锚定
	sandbox.PauseSession("T-9005")
	go func() {
		time.Sleep(100 * time.Millisecond)
		sandbox.ResumeSession("T-9005")
	}()
	if err := sandbox.GateFor("T-9005").WaitFrozen(context.Background(), "T-9005"); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(filepath.Join(dir, "tasklog", "T-9005", ".tasklog", "events.jsonl"))
	if len(strings.Split(strings.TrimRight(string(raw2), "\n"), "\n")) != len(lines) {
		t.Error("SSE 无增长的重复冻结不得重复锚定（baton 防重）")
	}
	sandbox.ClearFrozen("T-9005") // 释放后挂起不再锚定（注册表机制已在 sandbox 包测覆盖）
}

// ---- R5 帧时间戳（TP17-T3）----

// writeRaw 帧首注释格式契约：event: 行前缀 ": ts <RFC3339Nano>"（SSE 合法注释，
// 旧解析器跳过）；非 event 行不加。
func TestWriteRawTsAnnotation(t *testing.T) {
	dir := t.TempDir()
	entry := &aiLogEntry{rawPath: filepath.Join(dir, "t.sse.log")}
	entry.writeRaw([]byte("event: bridge.hello\n"))
	entry.writeRaw([]byte("data: {\"model\":\"m\"}\n"))
	entry.writeRaw([]byte("\n"))
	entry.writeRaw([]byte(": ping\n"))

	raw, err := os.ReadFile(entry.rawPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], ": ts ") {
		t.Fatalf("event 行前应有 ts 注释: %q", lines)
	}
	ts := strings.TrimPrefix(lines[0], ": ts ")
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts 应为 RFC3339Nano: %q (%v)", ts, err)
	}
	if lines[1] != "event: bridge.hello" || lines[2] != "data: {\"model\":\"m\"}" {
		t.Errorf("帧原文不得被改写: %q", lines[1:3])
	}
	if strings.HasPrefix(lines[3], ": ts") || strings.HasPrefix(lines[4], ": ts 2") {
		t.Errorf("非 event 行不得加注释: %q", lines[3:])
	}
}

// TestBatonTsPassthrough_RealBinary — R5 端到端：engine 写出的 .sse.log 格式 →
// baton 投影 → 帧驱动事件 ts=帧接收时刻（≠投影时刻）。本机有 baton 兄弟仓时执行。
func TestBatonTsPassthrough_RealBinary(t *testing.T) {
	repoRoot := "../../../.."
	batonRepo := filepath.Join(repoRoot, "..", "baton")
	if _, err := os.Stat(filepath.Join(batonRepo, "go.mod")); err != nil {
		t.Skipf("无 baton 兄弟仓（%s）——CI 布局", batonRepo)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("本机无 go 工具链")
	}
	bin := filepath.Join(t.TempDir(), "baton")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/baton")
	build.Dir = batonRepo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 baton 失败: %v\n%s", err, out)
	}

	// engine writeRaw 产 .sse.log（真实写出路径，非手拼夹具）
	dir := t.TempDir()
	ssePath := filepath.Join(dir, "T-9006.sse.log")
	entry := &aiLogEntry{rawPath: ssePath}
	entry.writeRaw([]byte("event: bridge.hello\n"))
	entry.writeRaw([]byte(`data: {"provider":"dsh","model":"m"}` + "\n"))
	entry.writeRaw([]byte("\n"))
	entry.writeRaw([]byte("event: session.event\n"))
	entry.writeRaw([]byte(`data: {"sessionId":"main","event":{"type":"user/message","data":{"content":[{"type":"text","text":"x"}]}}}` + "\n"))
	entry.writeRaw([]byte("\n"))

	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	batonProjectTurn(batonPen{TaskID: "T-9006", Entry: &aiLogEntry{rawPath: ssePath}, Origin: nil, Agent: ""}, "对项目做安全审计")

	raw, err := os.ReadFile(filepath.Join(dir, "tasklog", "T-9006", ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// 帧驱动事件（user/message，seq≥3）：ts 必须是帧时刻（纳秒精度）而非投影时刻
	for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if !strings.Contains(ln, `"role":"user"`) {
			continue
		}
		i := strings.Index(ln, `"ts":"`)
		ts := ln[i+6 : i+6+strings.Index(ln[i+6:], `"`)]
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Errorf("帧驱动事件 ts 非法: %q", ts)
		}
		if !strings.Contains(ts, ".") {
			t.Errorf("帧驱动事件 ts 应为帧时刻(纳秒精度), 疑似投影时刻降级: %q", ts)
		}
		return
	}
	t.Fatal("未找到 user 事件")
}

// TestBatonOriginPassthrough_RealBinary — R7 完整版：tenant/user/constraints 旗标
// 透传 → 投影档案 envelope/spine。本机有 baton 兄弟仓时执行。
func TestBatonOriginPassthrough_RealBinary(t *testing.T) {
	repoRoot := "../../../.."
	batonRepo := filepath.Join(repoRoot, "..", "baton")
	if _, err := os.Stat(filepath.Join(batonRepo, "go.mod")); err != nil {
		t.Skipf("无 baton 兄弟仓（%s）——CI 布局", batonRepo)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("本机无 go 工具链")
	}
	bin := filepath.Join(t.TempDir(), "baton")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/baton")
	build.Dir = batonRepo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 baton 失败: %v\n%s", err, out)
	}
	dir := t.TempDir()
	t.Setenv("BATON_BIN", bin)
	t.Setenv("CODEAUDIT_TASKLOG_DIR", filepath.Join(dir, "tasklog"))
	ssePath := writeSSE(t, dir, "T-9007.sse.log")

	origin := &pb.TaskOrigin{Tenant: "acme", User: "xiaowang", Constraints: []string{"不动支付网关SDK", "回归测试全绿"}}
	batonProjectTurn(batonPen{TaskID: "T-9007", Entry: &aiLogEntry{rawPath: ssePath}, Origin: origin, Agent: ""}, "对项目做安全审计")

	raw, err := os.ReadFile(filepath.Join(dir, "tasklog", "T-9007", ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"tenant":"acme"`, `"user":"xiaowang"`,
		`"constraints":["不动支付网关SDK","回归测试全绿"]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("档案缺 origin 维度 %s:\n%s", want, raw)
		}
	}
	spine, err := os.ReadFile(filepath.Join(dir, "tasklog", "T-9007", ".tasklog", "spine.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spine), "回归测试全绿") {
		t.Errorf("spine.constraints 未落: %s", spine)
	}
}

// 对抗式审查 P3-7（5427d224 两修复的回归锚）：pen 随任务级 agent（M105 牙齿）
// ——dsh 路径换手后 baton project -agent 必须传任务级新 agent（agentOrEnv 归一）。
func TestBatonProjectTurn_PenFromTaskAgent(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("BATON_BIN", stubBaton(t, argsFile))
	t.Setenv("CODEAUDIT_TASKLOG_DIR", dir)
	sse := writeSSE(t, dir, "T-pen1.sse.log")
	batonProjectTurn(batonPen{TaskID: "T-pen1", Entry: &aiLogEntry{rawPath: sse}, Origin: nil, Agent: "maka-acp"}, "goal")
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "-agent\nmaka-acp") { // stub 每参数一行
		t.Fatalf("pen 未随任务级 agent（换手档案 pen 失真回归）: %s", string(raw))
	}
	// 空参数回落 env 缺省（4a/4b 与图路径现状）
	batonProjectTurn(batonPen{TaskID: "T-pen2", Entry: &aiLogEntry{rawPath: sse}, Origin: nil, Agent: ""}, "goal")
	raw2, _ := os.ReadFile(argsFile)
	if strings.Contains(string(raw2), "T-pen2") && !strings.Contains(string(raw2), "-agent\ndsh") {
		t.Fatalf("空 agent 应回落缺省 dsh: %s", string(raw2))
	}
}

// 对抗式审查 P3-7：粒度刷新锚（M104 牙齿）——同任务换方言（换笔）后条目粒度
// 必须随最新回合方言（TURN 型接续不得残留 TOKEN 声明）。
func TestWireAILog_GranularityRefreshesOnDialectChange(t *testing.T) {
	sharedAILogs = newAILogStore("")
	cfgDsh := &sandbox.Config{Mode: "openshell"}
	cfgDsh.Agent.Dialect = "dsh-sse"
	e := wireAILog(cfgDsh, "T-gran")
	if g := e.granularityNow(); g != pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN {
		t.Fatalf("首建粒度: %v", g)
	}
	cfgMaka := &sandbox.Config{Mode: "openshell"}
	cfgMaka.Agent.Dialect = "maka-runtime-host"
	e2 := wireAILog(cfgMaka, "T-gran") // 同 task：writer 幂等返回同 entry
	if e2 != e {
		t.Fatal("同任务应返回同条目")
	}
	if g := e.granularityNow(); g != pb.AILogGranularity_AI_LOG_GRANULARITY_TURN {
		t.Fatalf("换笔后粒度未刷新（TURN 型接续残留 TOKEN=前端逐字实时误导）: %v", g)
	}
}

// R132 牙齿：多纪元复位锚——上一纪元 defer finish() 置 complete 后，新 Run 重接线
// （同任务幂等 writer：pause/resume、切换 handoff-retry、auto-retry 皆重入）必须
// 复位 false。不复位则一切订阅读到永真 complete=纪元 N+1 字节永不送达（网关
// re-arm 后首读即被终帧关流；ui_check 切换全路径实证面板冻结）。
func TestWireAILog_CompleteResetsOnRewire(t *testing.T) {
	sharedAILogs = newAILogStore("")
	cfg := &sandbox.Config{Mode: "openshell"}
	cfg.Agent.Dialect = "dsh-sse"
	e := wireAILog(cfg, "T-epoch")
	e.write([]byte("epoch-1"))
	e.finish()
	if _, _, complete, total := e.read(0, 0); !complete || total != 7 {
		t.Fatalf("纪元1 收束态缺失: complete=%v total=%d", complete, total)
	}
	if e2 := wireAILog(cfg, "T-epoch"); e2 != e {
		t.Fatal("同任务应返回同条目")
	}
	_, next, complete, total := e.read(7, 0)
	if complete {
		t.Fatal("重接线后 complete 未复位——纪元2 订阅者被永真终态冻结")
	}
	if next != 7 || total != 7 {
		t.Fatalf("复位不得动字节面: next=%d total=%d", next, total)
	}
}

// M107 牙齿（对抗式审查轴1/轴3：P2-4 分叉透传零锚）：maka 方言分叉的 pen 必须
// 随任务级 agent——回退为恒 makaAgentID() 本用例红。stub 设施加同 writeSSE。
func TestBatonProjectTurn_MakaForkPen(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("BATON_BIN", stubBaton(t, argsFile))
	t.Setenv("CODEAUDIT_TASKLOG_DIR", dir)
	sse := writeSSE(t, dir, "T-mk1.sse.log")
	// maka-events.jsonl 存在且非空 → batonProjectTurn 走 maka 分叉
	if err := os.MkdirAll(filepath.Join(dir, "T-mk1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "T-mk1", "maka-events.jsonl"), []byte("{\"seq\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	batonProjectTurn(batonPen{TaskID: "T-mk1", Entry: &aiLogEntry{rawPath: sse}, Origin: nil, Agent: "maka-acp"}, "goal")
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "-agent\nmaka-acp") { // stub 每参数一行
		t.Fatalf("maka 分叉 pen 未随任务级 agent（方言路径透传回退）: %s", string(raw))
	}
}

// M108 牙齿（-race）：granularityNow 持锁读——去锁变异在并发读写下必触 race。
func TestGranularityNow_RaceSafety(t *testing.T) {
	sharedAILogs = newAILogStore("")
	e := sharedAILogs.writer("T-race", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	done := make(chan struct{})
	go func() { // 并发写侧（模拟 wireAILog 换笔刷新）
		defer close(done)
		for i := 0; i < 2000; i++ {
			e.mu.Lock()
			e.granularity = pb.AILogGranularity_AI_LOG_GRANULARITY_TURN
			e.mu.Unlock()
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = e.granularityNow() // 读侧：去锁变异此处与写侧构成数据竞争（-race 判杀）
	}
	<-done
}
