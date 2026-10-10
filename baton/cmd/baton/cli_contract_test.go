package main

// CLI 契约锁定测试：docs/api-external.md 是 CLI
// 面契约 SSOT——退出码三态（0 成功/1 运行失败/2 用法错）、stdout 形状、usage 上
// stderr。engine baton_hook 以子进程方式消费本面（只看输出+退出码），此前零锁定。
// run* 已函数化返回退出码（os.Exit 收口 main），测试直接调用+管道捕获输出。

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture — 捕获 stdout/stderr 到退出码结算（同测例内多段捕获取并集）。
func capture(t *testing.T, fn func()) (out, errOut string) {
	t.Helper()
	or, ow, _ := os.Pipe()
	er, ew, _ := os.Pipe()
	oldO, oldE := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	fn()
	ow.Close()
	ew.Close()
	os.Stdout, os.Stderr = oldO, oldE
	o, oerr := io.ReadAll(or)
	e, eerr := io.ReadAll(er)
	if oerr != nil || eerr != nil {
		t.Fatalf("pipe read: %v/%v", oerr, eerr)
	}
	return string(o), string(e)
}

// withArgs — 隔离 os.Args（usage 分支不打 os.Args，本测试组无需；留作形状完整性）。
func withArgs(t *testing.T, args []string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"baton"}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestCLI_ProjectContract(t *testing.T) {
	// 缺必填 → 2 + usage 上 stderr（api-external §0）
	var code int
	var out, errOut string
	out, errOut = capture(t, func() { code = runProject([]string{"-sse", "x.sse.log"}) })
	if code != 2 || !strings.Contains(errOut, "baton project -sse") || out != "" {
		t.Fatalf("缺必填: code=%d out=%q err=%q", code, out, errOut)
	}
	// -now 非法 → 2
	out, errOut = capture(t, func() {
		code = runProject([]string{"-sse", "a.sse.log", "-out", t.TempDir(), "-task", "T", "-goal", "g", "-now", "not-rfc3339"})
	})
	if code != 2 || !strings.Contains(errOut, "-now 非法 RFC3339") {
		t.Fatalf("-now 校验: code=%d err=%q", code, errOut)
	}
	// 非法输入（.txt 后缀）→ 1 + "投影失败:" 前缀（§1 退出 1 样例族）
	dir := t.TempDir()
	bad := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut = capture(t, func() {
		code = runProject([]string{"-sse", bad, "-out", dir, "-task", "T", "-goal", "g"})
	})
	if code != 1 || !strings.Contains(errOut, "投影失败:") {
		t.Fatalf("运行失败: code=%d err=%q", code, errOut)
	}
	// 成功 → 0 + stdout 首行 "events=N blobs=M"（§1 输出形状）
	sse := filepath.Join(dir, "basic.sse.log")
	src, err := os.ReadFile("../../internal/project/testdata/basic.sse.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sse, src, 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	out, _ = capture(t, func() {
		code = runProject([]string{"-sse", sse, "-out", ws, "-task", "T-1042", "-goal", "目标", "-now", "2026-09-19T00:00:00Z"})
	})
	if code != 0 {
		t.Fatalf("成功路径 code=%d", code)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, "events=") || !strings.Contains(first, "blobs=") {
		t.Fatalf("stdout 首行形状 = %q（api-external §1）", first)
	}
	// -graph 第二输入（ADR-001）：双输入成功 + graph.* 入档
	gl := filepath.Join(dir, "basic.graph.log")
	gsrc, err := os.ReadFile("../../internal/project/testdata/graph_basic.graph.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gl, gsrc, 0o644); err != nil {
		t.Fatal(err)
	}
	wsG := filepath.Join(dir, "ws-graph")
	out, _ = capture(t, func() {
		code = runProject([]string{"-sse", sse, "-graph", gl, "-out", wsG, "-task", "T-1042", "-goal", "目标", "-now", "2026-09-19T00:00:00Z"})
	})
	if code != 0 {
		t.Fatalf("-graph 成功路径 code=%d err=%q", code, out)
	}
	if !strings.Contains(out, "graph.node_declared") {
		t.Fatalf("-graph 投影须含 graph.* 类型（ADR-001），stdout=%q", out)
	}
	if !strings.Contains(out, "task.created") { // ByType 逐行段
		t.Fatalf("stdout 缺 ByType 段: %q", out)
	}
}

func TestCLI_BriefContract(t *testing.T) {
	// 未知方言 → 2；缺 -in → 2
	var code int
	var out, errOut string
	out, errOut = capture(t, func() { code = runBrief([]string{"-in", "x", "-format", "nope"}) })
	if code != 2 || !strings.Contains(errOut, "baton brief -in") {
		t.Fatalf("未知方言: code=%d err=%q", code, errOut)
	}
	// 先投影出档案再渲染（链路内联夹具，与 brief_test.ChainFromProjector 互补：
	// 这里锁的是 CLI 层缝合而非 Render 语义）
	dir := t.TempDir()
	sse := filepath.Join(dir, "basic.sse.log")
	src, err := os.ReadFile("../../internal/project/testdata/basic.sse.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sse, src, 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	capture(t, func() {
		code = runProject([]string{"-sse", sse, "-out", ws, "-task", "T", "-goal", "g", "-now", "2026-09-19T00:00:00Z"})
	})
	if code != 0 {
		t.Fatalf("投影前置失败 code=%d", code)
	}
	out, _ = capture(t, func() { code = runBrief([]string{"-in", ws, "-format", "maka", "-tail", "3"}) })
	if code != 0 || !strings.Contains(out, "baton 交接简报") {
		t.Fatalf("brief 成功: code=%d out 前 80=%q", code, out[:min(80, len(out))])
	}
	// 档案缺失 → 1 + "渲染失败:"
	out, errOut = capture(t, func() { code = runBrief([]string{"-in", filepath.Join(dir, "void"), "-format", "dsh"}) })
	if code != 1 || !strings.Contains(errOut, "渲染失败:") {
		t.Fatalf("档案缺失: code=%d err=%q", code, errOut)
	}
}

func TestCLI_HandoffContract(t *testing.T) {
	var code int
	var out, errOut string
	// 缺 -in / 非法 phase → 2
	out, errOut = capture(t, func() { code = runHandoff([]string{"-phase", "checkpointed"}) })
	if code != 2 || !strings.Contains(errOut, "baton handoff -in") {
		t.Fatalf("缺 -in: code=%d err=%q", code, errOut)
	}
	out, errOut = capture(t, func() { code = runHandoff([]string{"-in", "x", "-phase", "seeded"}) })
	if code != 2 {
		t.Fatalf("非法 phase: code=%d", code)
	}
	// 档案缺失 → 1 + "换手锚定失败:"
	dir := t.TempDir()
	out, errOut = capture(t, func() { code = runHandoff([]string{"-in", dir}) })
	if code != 1 || !strings.Contains(errOut, "换手锚定失败:") {
		t.Fatalf("无档案: code=%d err=%q", code, errOut)
	}
	// 成功 → "checkpointed seq=N"；重复调用 → "skipped: 基线已锚定"（幂等防重双形态，§3）
	sse := filepath.Join(dir, "basic.sse.log")
	src, err := os.ReadFile("../../internal/project/testdata/basic.sse.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sse, src, 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	capture(t, func() {
		code = runProject([]string{"-sse", sse, "-out", ws, "-task", "T", "-goal", "g", "-now", "2026-09-19T00:00:00Z"})
	})
	out, _ = capture(t, func() {
		code = runHandoff([]string{"-in", ws, "-now", "2026-09-19T00:01:00Z"})
	})
	if code != 0 || !strings.HasPrefix(out, "checkpointed seq=") {
		t.Fatalf("首次锚定: code=%d out=%q", code, out)
	}
	out, _ = capture(t, func() {
		code = runHandoff([]string{"-in", ws, "-now", "2026-09-19T00:02:00Z"})
	})
	if code != 0 || !strings.HasPrefix(out, "skipped: 基线已锚定") {
		t.Fatalf("幂等防重: code=%d out=%q（api-external §3 双形态）", code, out)
	}
}

func TestCLI_MainDispatch(t *testing.T) {
	// 顶层派发缺子命令 → 2 + usage（main 直收 os.Exit，间接锁：usage 文案含三子命令）
	_, errOut := capture(t, func() {
		defer func() {
			if r := recover(); r != nil { // main 在测试进程内不可直接调（os.Exit）——此例锁 usage() 输出面
				t.Fatalf("usage panic: %v", r)
			}
		}()
		usage()
	})
	if !strings.Contains(errOut, "baton project") || !strings.Contains(errOut, "baton brief") || !strings.Contains(errOut, "baton handoff") {
		t.Fatalf("usage 须列三子命令: %q", errOut)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
