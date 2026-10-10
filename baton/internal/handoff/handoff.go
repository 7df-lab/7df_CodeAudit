// Package handoff 实现换手事件的 append 面（M4 基线锚，TP17-T2 联动）。
// events.jsonl 双段语义：SSE 派生段（project 全量重写可重算）+ 尾部 handoff.*
// 生命周期段（append-only 事实，project 保留）。checkpointed 由引擎在 gate 冻结
// 点调用（真实定格时刻：回合已收束、SSE 已停止增长）。
package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"baton/internal/model"
)

// Options checkpointed 锚定输入。git-anchor/tree-ref 为工作区锚（R3），
// 引擎侧可得则传（v0：git-anchor=源码树 HEAD；tree-ref 待沙箱导出链）。
type Options struct {
	InRoot    string // 任务工作区根（含 .tasklog/）
	GitAnchor string
	TreeRef   string
	Now       time.Time // 零值=当前时刻（测试可固定）
}

// Checkpoint — append 一条 handoff.checkpointed{log_seq=当前事件行数}。
// 幂等防重：尾部已是同 log_seq 的 checkpointed（重试场景）则跳过返回 (seq, true)。
// 档案缺失（.tasklog/ 或 events.jsonl 不存在）= 无基线可锚，返回错误由调用方降级。
func Checkpoint(opts Options) (seq int, skipped bool, err error) {
	dir := filepath.Join(opts.InRoot, ".tasklog")
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return 0, false, fmt.Errorf("读 events.jsonl: %w", err)
	}
	var lines []string
	var evs []model.Event
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln == "" {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			return 0, false, fmt.Errorf("events.jsonl 第 %d 行非法: %w", len(lines)+1, err)
		}
		lines = append(lines, ln)
		evs = append(evs, ev)
	}
	// R121 P3：seq 连续性守卫——log_seq 锚与幂等防重都以"seq=行号"为前提，外部
	// 写入造成 seq 有洞/重号时锚会静默失准（fail-closed：档案异常宁可拒绝锚定）。
	for i, ev := range evs {
		if ev.Seq != i+1 {
			return 0, false, fmt.Errorf("events.jsonl 第 %d 行 seq=%d 与行号不符（档案有洞/重号）——锚定拒绝", i+1, ev.Seq)
		}
	}
	if len(evs) == 0 {
		return 0, false, fmt.Errorf("events.jsonl 为空：无基线可锚")
	}

	// 防重：尾部 checkpointed 锚定的就是当前全量（其后无新事件=SSE 无增长的
	// 重复锚定，冻结回调重试场景）→ 幂等跳过。SSE 已增长则尾部锚必落后，正常 append。
	if last := evs[len(evs)-1]; last.Type == "handoff.checkpointed" &&
		last.HandoffCheckpoint != nil && last.HandoffCheckpoint.LogSeq == len(evs)-1 {
		return last.Seq, true, nil
	}

	last := evs[len(evs)-1]
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	ev := model.Event{
		Seq: len(evs) + 1, Ts: now.UTC().Format(time.RFC3339),
		Task: last.Task, Tenant: last.Tenant, User: last.User,
		Session: "main", Pen: last.Pen,
		Type: "handoff.checkpointed",
		HandoffCheckpoint: &model.HandoffCheckpoint{
			LogSeq:    len(evs),
			Workspace: model.WorkspaceMarker{GitAnchor: opts.GitAnchor, TreeRef: opts.TreeRef},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return 0, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return 0, false, err
	}
	if err := f.Sync(); err != nil { // 锚是事实记录：落盘才算锚上（页缓存丢失窗口一行消除）
		return 0, false, err
	}
	return ev.Seq, false, nil
}
