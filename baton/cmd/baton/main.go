// baton — 换 agent 不丢任务的交接协议。子命令：project（M2 投影器：DSH
// .sse.log → .tasklog/）、brief（M3 简报渲染器：.tasklog/ → 交接简报）。
// 零第三方依赖（细腰的可移植性是产品属性，见 README §1）。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"baton/internal/brief"
	"baton/internal/handoff"
	"baton/internal/project"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "project":
		os.Exit(runProject(os.Args[2:]))
	case "brief":
		os.Exit(runBrief(os.Args[2:]))
	case "handoff":
		os.Exit(runHandoff(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

// runProject 返回进程退出码（0/1/2，api-external.md §0/§1 契约；os.Exit 收口
// main 使本函数可直接被 CLI 契约测试调用）。
func runProject(args []string) int {
	fs := flag.NewFlagSet("baton project", flag.ExitOnError)
	sse := fs.String("sse", "", "引擎 .sse.log 路径（DSH 方言输入；与 -maka 二选一）")
	maka := fs.String("maka", "", "Maka RuntimeEventLog JSONL 路径（maka 方言输入；与 -sse/-acp 三选一）")
	acp := fs.String("acp", "", "ACP 帧流 JSONL 路径（acp-out.jsonl——engine acpSession 驱动器逐帧留存；与 -sse/-maka 三选一，2026-09-24）")
	graph := fs.String("graph", "", "图任务第二输入 <taskID>.graph.log（空=非图任务；ts 归并 graph-first，ADR-001）")
	out := fs.String("out", "", "任务工作区根目录；产物写 <out>/.tasklog/（必填）")
	task := fs.String("task", "", "task_id（必填，换手不变更）")
	tenant := fs.String("tenant", "", "租户")
	user := fs.String("user", "", "发起用户")
	goal := fs.String("goal", "", "任务目标（必填，脊柱 goal 来源）")
	constraints := repeated(fs, "constraint", "任务约束（可多次传入）")
	agent := fs.String("agent", "dsh", "持笔 harness 标识")
	model := fs.String("model", "", "覆盖 bridge.hello 的模型名（默认取流内值）")
	threshold := fs.Int("blob-threshold", 2048, "大载荷 blob 阈值（字节）")
	wsID := fs.String("workspace-id", "", "固定工作区 UUID（默认随机；测试用）")
	gitAnchor := fs.String("git-anchor", "", "工作区 git 基准 commit")
	now := fs.String("now", "", "固定时间源 RFC3339（测试用，默认当前时刻）")
	_ = fs.Parse(args)

	if *out == "" || *task == "" || *goal == "" {
		usage()
		return 2
	}
	// 三输入恰好一个非空（单一计数路径；2026-09-22 等价简化：原 case 卫式经布尔
	// 展开为永真式——真校验自始只有此计数，删卫式不改变任何输入的判定结果）。
	n := 0
	for _, v := range []string{*sse, *maka, *acp} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		fmt.Fprintln(os.Stderr, "-sse/-maka/-acp 恰好选一（DSH 原始帧 / Maka RuntimeEventLog / ACP 帧流）")
		return 2
	}

	opts := project.Options{
		SSEPath: *sse, MakaPath: *maka, ACPPath: *acp, GraphPath: *graph, OutRoot: *out,
		Task: *task, Tenant: *tenant, User: *user, Goal: *goal,
		Constraints: *constraints, Agent: *agent, Model: *model,
		BlobThreshold: *threshold, WorkspaceID: *wsID, GitAnchor: *gitAnchor,
	}
	if *now != "" {
		t, err := time.Parse(time.RFC3339, *now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-now 非法 RFC3339: %v\n", err)
			return 2
		}
		opts.Now = t
	}

	stats, err := project.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "投影失败: %v\n", err)
		return 1
	}
	fmt.Printf("events=%d blobs=%d\n", stats.Events, stats.Blobs)
	if stats.Parked > 0 {
		fmt.Printf("  PARKED=%d（金丝雀：schema 漂移行已计档，.tasklog/PARKED；简报将标注不确定窗口）\n", stats.Parked)
	}
	// ByType 按名升序输出（map 迭代序随机——同输入两次运行 stdout 不同，与档案
	// 确定性契约同源的 stdout 面收口；api-external.md §1 同步）。
	types := make([]string, 0, len(stats.ByType))
	for typ := range stats.ByType {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		fmt.Printf("  %-18s %d\n", typ, stats.ByType[typ])
	}
	for _, w := range stats.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	return 0
}

// runBrief 返回退出码（api-external.md §2 契约）。
func runBrief(args []string) int {
	fs := flag.NewFlagSet("baton brief", flag.ExitOnError)
	in := fs.String("in", "", "任务工作区根目录（读 <in>/.tasklog/，必填）")
	format := fs.String("format", "", "简报方言：maka=初始 prompt | dsh=任务卡前置段（必填）")
	tail := fs.Int("tail", 20, "尾部逐字事件条数（0=省略该段）")
	out := fs.String("out", "", "输出文件路径（默认 stdout）")
	_ = fs.Parse(args)

	if *in == "" || *format == "" {
		usage()
		return 2
	}
	// 方言枚举 CLI 层预校验（api-external §0"非法枚举=用法错 2"；同 handoff
	// phase 先例——此前透传 Render 报 1，与 §0 契约矛盾被 CLI 契约测试抓出）。
	if *format != "maka" && *format != "dsh" {
		usage()
		return 2
	}
	text, err := brief.Render(brief.Options{InRoot: *in, Format: *format, Tail: *tail})
	if err != nil {
		fmt.Fprintf(os.Stderr, "渲染失败: %v\n", err)
		return 1
	}
	if *out == "" {
		fmt.Print(text)
		return 0
	}
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写 %s 失败: %v\n", *out, err)
		return 1
	}
	return 0
}

// runHandoff — 换手事件 append 面（v0: checkpointed——引擎在 gate 冻结点调用，
// 锚定 log_seq 与工作区 git-anchor/tree-ref）。
// runHandoff 返回退出码（api-external.md §3 契约）。
func runHandoff(args []string) int {
	fs := flag.NewFlagSet("baton handoff", flag.ExitOnError)
	in := fs.String("in", "", "任务工作区根目录（读 <in>/.tasklog/，必填）")
	phase := fs.String("phase", "checkpointed", "换手阶段（v0 仅 checkpointed）")
	gitAnchor := fs.String("git-anchor", "", "工作区 git 基准 commit（R3，可得则传）")
	treeRef := fs.String("tree-ref", "", "工作区快照桶引用（R3，可得则传）")
	now := fs.String("now", "", "固定时间源 RFC3339（测试用，默认当前时刻）")
	_ = fs.Parse(args)

	if *in == "" || *phase != "checkpointed" {
		usage()
		return 2
	}
	opts := handoff.Options{InRoot: *in, GitAnchor: *gitAnchor, TreeRef: *treeRef}
	if *now != "" {
		t, err := time.Parse(time.RFC3339, *now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-now 非法 RFC3339: %v\n", err)
			return 2
		}
		opts.Now = t
	}
	seq, skipped, err := handoff.Checkpoint(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "换手锚定失败: %v\n", err)
		return 1
	}
	if skipped {
		fmt.Printf("skipped: 基线已锚定（seq=%d）\n", seq)
		return 0
	}
	fmt.Printf("checkpointed seq=%d\n", seq)
	return 0
}

// repeated 实现可重复字符串旗标（-constraint a -constraint b）。
func repeated(fs *flag.FlagSet, name, usage string) *[]string {
	var v []string
	fs.Var(&repeatVal{&v}, name, usage)
	return &v
}

type repeatVal struct{ p *[]string }

func (r repeatVal) String() string { return "" }
func (r repeatVal) Set(s string) error {
	*r.p = append(*r.p, s)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: baton project -sse <path.sse.log> -out <workspaceRoot> -task <id> -goal <text>
                 [-graph <path.graph.log> -tenant -user -constraint …可重复 -agent -model
                  -blob-threshold -workspace-id -git-anchor -now RFC3339]
       baton brief -in <workspaceRoot> -format maka|dsh [-tail 20] [-out <file>]
       baton handoff -in <workspaceRoot> [-phase checkpointed] [-git-anchor X -tree-ref Y -now RFC3339]`)
}
