// Package brief 实现简报渲染器（M3）：读 .tasklog/ 档案渲染交接简报。
// 种子契约（README §6）：简报 = 脊柱渲染 + 尾部逐字，对所有 agent 一致；
// 方言（maka=初始 prompt、dsh=任务卡前置段）只差标题包装层级，内核同一。
//
// 认识论纪律：不自动提炼 facts/decisions——"已验证"是语义判断，错误的事实
// 比没有更危险（schema 认识论分层在简报侧的落点）；脊柱有什么渲染什么。
// 机械可投影的状态（任务终态、indeterminate 调用、换手史）无损呈现。
package brief

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"baton/internal/model"
)

// Options 简报渲染输入。Tail=0 省略尾部逐字段（默认值由 CLI 层定为 20）。
type Options struct {
	InRoot string // 任务工作区根（读 <InRoot>/.tasklog/）
	Format string // maka | dsh
	Tail   int    // 尾部逐字事件条数
}

// profile 方言包装：maka 是自包含全文（H1 起），dsh 嵌任务卡前置段（H2 起，
// 小节降一级避免与任务卡自身结构冲突）。
type profile struct {
	top   string
	sub   string
	title string
}

var profiles = map[string]profile{
	"maka": {"#", "##", "baton 交接简报"},
	"dsh":  {"##", "###", "baton 交接简报（前任进度）"},
}

// Render 渲染简报。纯读档案、无副作用、无时间源注入——同档案必出同文本。
func Render(opts Options) (string, error) {
	prof, ok := profiles[opts.Format]
	if !ok {
		return "", fmt.Errorf("未知方言 %q（可选：maka=初始 prompt、dsh=任务卡前置段）", opts.Format)
	}
	dir := filepath.Join(opts.InRoot, ".tasklog")
	rawLines, evs, err := loadEvents(dir)
	if err != nil {
		return "", err
	}
	spine, err := loadJSON(dir, "spine.json", &model.Spine{})
	if err != nil {
		return "", err
	}
	ws, err := loadJSON(dir, "WORKSPACE.json", &model.WorkspaceFile{})
	if err != nil {
		return "", err
	}

	// 金丝雀不确定窗口（README §5）：档案含 PARKED 标记=maka 转译存在 schema
	// 漂移行——简报如实标注（消费方据此降低对档案完整性的信任权重）。
	parked, _ := os.ReadFile(filepath.Join(dir, "PARKED"))

	r := &renderer{prof: prof, sb: &strings.Builder{}}
	fmt.Fprintf(r.sb, "%s %s\n\n", prof.top, prof.title)
	r.meta(spine, ws, evs)
	if len(parked) > 0 {
		fmt.Fprintf(r.sb, "\n### ⚠ 投影不确定窗口（金丝雀 PARKED）\n\n%s\n", strings.TrimRight(string(parked), "\n"))
	}
	r.spineSections(spine)
	r.stateSections(evs)
	r.tail(rawLines, opts.Tail)

	s := r.sb.String()
	return strings.TrimRight(s, "\n") + "\n", nil
}

type renderer struct {
	prof profile
	sb   *strings.Builder
}

func (r *renderer) section(title string) {
	fmt.Fprintf(r.sb, "%s %s\n\n", r.prof.sub, title)
}

// meta 档案元信息：身份、前任、新鲜度、档案指引（接手 agent 的寻址入口）。
func (r *renderer) meta(sp *model.Spine, ws *model.WorkspaceFile, evs []model.Event) {
	last := evs[len(evs)-1]
	wsLine := ws.WorkspaceID
	if ws.GitAnchor != "" {
		wsLine += fmt.Sprintf("（git anchor: %s）", ws.GitAnchor)
	}
	pen := last.Pen.Agent
	if last.Pen.Model != nil && *last.Pen.Model != "" {
		pen += " / " + *last.Pen.Model
	}
	fmt.Fprintf(r.sb, "- task: %s\n", sp.Task)
	fmt.Fprintf(r.sb, "- workspace: %s\n", wsLine)
	fmt.Fprintf(r.sb, "- 前任: %s\n", pen)
	fmt.Fprintf(r.sb, "- 档案: %d 条事件（seq 1–%d），截至 %s\n", len(evs), len(evs), last.Ts)
	r.sb.WriteString("- 全档案在工作区 .tasklog/：events.jsonl 全史、spine.json 脊柱快照、")
	r.sb.WriteString("blobs/ 大载荷正文（事件行内 blob: 引用在此解析）\n\n")
}

// spineSections 脊柱渲染，字段序对齐 spine.schema.json；空段整段省略。
func (r *renderer) spineSections(sp *model.Spine) {
	if sp.Goal != "" {
		r.section("目标")
		r.sb.WriteString(sp.Goal + "\n\n")
	}
	if len(sp.Constraints) > 0 {
		r.section("约束")
		r.list(sp.Constraints)
	}
	if len(sp.Facts) > 0 {
		r.section("已验证事实")
		for _, f := range sp.Facts {
			line := "- " + f.Text
			if len(f.Evidence) > 0 {
				line += evidenceSuffix(f.Evidence)
			}
			r.sb.WriteString(line + "\n")
		}
		r.sb.WriteByte('\n')
	}
	if len(sp.Decisions) > 0 {
		r.section("决策")
		for _, d := range sp.Decisions {
			fmt.Fprintf(r.sb, "- [%s] %s %s\n", d.Status, d.ID, d.Text)
		}
		r.sb.WriteByte('\n')
	}
	if len(sp.Open) > 0 {
		r.section("未决问题")
		r.list(sp.Open)
	}
	if sp.Hypothesis != "" {
		r.section("当前假设")
		r.sb.WriteString(sp.Hypothesis + "\n\n")
	}
	if sp.Next != "" {
		r.section("下一步")
		r.sb.WriteString(sp.Next + "\n\n")
	}
}

// stateSections 机械状态段：任务终态、需复核调用、换手史——全部无损投影。
func (r *renderer) stateSections(evs []model.Event) {
	calls := map[string]string{} // call_id → tool 名（indeterminate 行需要人话）
	var status *model.Event
	var indets []string
	var reqs []model.HandoffRequested
	done := 0
	for i := range evs {
		ev := &evs[i]
		switch {
		case ev.Call != nil:
			calls[ev.Call.CallID] = ev.Call.Tool
		case ev.Type == "task.status":
			status = ev
		case ev.Result != nil && ev.Result.Outcome == "indeterminate":
			tool := calls[ev.Result.CallID]
			if tool == "" {
				tool = "（未知工具）"
			}
			indets = append(indets, fmt.Sprintf("- %s %s", ev.Result.CallID, tool))
		case ev.HandoffRequested != nil:
			reqs = append(reqs, *ev.HandoffRequested)
		case ev.HandoffDone != nil:
			done++
		}
	}
	if status != nil {
		r.section("任务状态")
		line := status.Status
		if status.Reason != "" {
			line += " — " + truncate(status.Reason, 200)
		}
		r.sb.WriteString(line + "\n\n")
	}
	if len(indets) > 0 {
		r.section("需复核调用（结果不明：缺失结果 ≠ 没跑过，接手先验证副作用）")
		for _, ln := range indets {
			r.sb.WriteString(ln + "\n")
		}
		r.sb.WriteByte('\n')
	}
	r.section("换手记录")
	if len(reqs) == 0 && done == 0 {
		r.sb.WriteString("- 首次换手（档案无 handoff 事件）\n\n")
		return
	}
	for _, hr := range reqs {
		fmt.Fprintf(r.sb, "- 请求: %s → %s（%s，发起: %s）\n",
			penLabel(hr.From), penLabel(hr.To), hr.Reason, hr.ByUser)
	}
	fmt.Fprintf(r.sb, "- 已完成: %d 次\n\n", done)
}

// tail 尾部逐字：events.jsonl 行原文不改写（含 blob: 引用），围栏防内容撞击
// （动态定长=内容最长反引号 run+1，最小 3——只升一级会被 4+ 反引号行撞破）。
func (r *renderer) tail(rawLines []string, n int) {
	if n <= 0 || len(rawLines) == 0 {
		return
	}
	lines := rawLines
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintf(r.sb, "%s 尾部事件（最近 %d 条，逐字原文；seq=行号=因果序）\n\n", r.prof.sub, len(lines))
	fenceLen := 3
	for _, ln := range lines {
		if run := longestBacktickRun(ln); run+1 > fenceLen {
			fenceLen = run + 1
		}
	}
	fence := strings.Repeat("`", fenceLen)
	r.sb.WriteString(fence + "jsonl\n")
	r.sb.WriteString(strings.Join(lines, "\n"))
	r.sb.WriteString("\n" + fence + "\n\n")
}

// longestBacktickRun 行内最长连续反引号数。
func longestBacktickRun(s string) int {
	max, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur = 0
		}
	}
	return max
}

func (r *renderer) list(items []string) {
	for _, it := range items {
		r.sb.WriteString("- " + it + "\n")
	}
	r.sb.WriteByte('\n')
}

func penLabel(ft model.HandoffFromTo) string {
	s := ft.Agent
	if ft.Model != nil && *ft.Model != "" {
		s += "/" + *ft.Model
	}
	return s
}

func evidenceSuffix(seqs []int) string {
	parts := make([]string, len(seqs))
	for i, s := range seqs {
		parts[i] = fmt.Sprintf("seq %d", s)
	}
	return "（evidence: " + strings.Join(parts, ", ") + "）"
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// loadEvents 读 events.jsonl：原文行（尾部逐字用）+ 解析事件（聚合用）同遍产出。
func loadEvents(dir string) ([]string, []model.Event, error) {
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, nil, fmt.Errorf("读 events.jsonl: %w", err)
	}
	var raw []string
	var evs []model.Event
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			return nil, nil, fmt.Errorf("events.jsonl 第 %d 行非法: %w", len(raw)+1, err)
		}
		raw = append(raw, ln)
		evs = append(evs, ev)
	}
	if len(evs) == 0 {
		return nil, nil, fmt.Errorf("events.jsonl 为空：无任务史可交接")
	}
	return raw, evs, nil
}

func loadJSON[T any](dir, name string, v *T) (*T, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("读 %s: %w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("%s 非法: %w", name, err)
	}
	return v, nil
}
