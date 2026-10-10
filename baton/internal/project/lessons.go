package project

// lessons.go — 机械提炼失败要点 → spine.facts（graph-reseed 共识 v0，零 token；
// 2026-09-20）。设计依据=主仓库 docs/designs/graph-reseed-task-handoff.md：
// 确定性（同事件流同产出——全量重算幂等）、去重（session+要点）、上限保最新
// （20 条，与 engine 注入侧 ≤600 字截断互补）。kind=lesson（教训）/observation
//（过程观察）；session 用 baton 原生维度（无 engine 词表，C4 干净）。

import (
	"encoding/json"
	"fmt"

	"baton/internal/model"
)

const (
	lessonCap       = 20  // facts 上限（保最新）
	errTextMaxBytes = 120 // 错误首行截断上限（字节；非行数）
)

// extractLessons — 从事件流机械提炼失败要点（projector 扫描完成后调用一次）。
func extractLessons(events []*model.Event) []model.Fact {
	var facts []model.Fact
	seen := map[string]bool{}
	add := func(f model.Fact) {
		key := f.Session + "\x00" + f.Text
		if seen[key] {
			return
		}
		seen[key] = true
		facts = append(facts, f)
	}
	// 末 K 工具调用观察（observation）不提炼——brief 认识论纪律=贫 spine（不自动
	// 造"已验证事实"，2026-09-20 TestChainFromProjector 实捕）；observation kind
	// 留 schema 占位。共识范围=失败要点（lesson）；设计档 v0 配方的"末 K 工具调用"
	// 项随此纪律弃做（主仓库设计档 §2 已同批回填）。
	for _, ev := range events {
		if ev.Result == nil {
			continue
		}
		r := ev.Result
		switch r.Outcome {
		case "indeterminate":
			add(model.Fact{Kind: "lesson", Session: ev.Session,
				Text:     fmt.Sprintf("回合中断：调用 %s 结果不明（缺失结果≠没跑过，接手先验证副作用）", r.CallID),
				Evidence: []int{ev.Seq}})
		case "error":
			if msg := firstErrLine(r); msg != "" {
				add(model.Fact{Kind: "lesson", Session: ev.Session,
					Text: "错误输出：" + msg, Evidence: []int{ev.Seq}})
			}
			if stringsContainsFold(firstErrLine(r), "REFUSED") {
				add(model.Fact{Kind: "lesson", Session: ev.Session,
					Text:     "工具拒绝执行（REFUSED）——检查策略面/参数合法性后再试",
					Evidence: []int{ev.Seq}})
			}
		}
	}
	if failedSeq := failedStatusSeq(events); failedSeq > 0 {
		add(model.Fact{Kind: "lesson",
			Text:     "任务曾以 FAILED 终态收束（runtime.exit）——失败要点见上方各条",
			Evidence: []int{failedSeq}})
	}
	// 上限保最新
	if len(facts) > lessonCap {
		facts = facts[len(facts)-lessonCap:]
	}
	return facts
}

// firstErrLine — 错误输出首行（Result.Result 为 %q 转义的 JSON 字符串）。
func firstErrLine(r *model.Result) string {
	if len(r.Result) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Result, &s); err != nil {
		s = string(r.Result)
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > errTextMaxBytes {
		s = s[:errTextMaxBytes] + "…"
	}
	return s
}

func stringsContainsFold(s, sub string) bool {
	n, m := len(s), len(sub)
	for i := 0; i+m <= n; i++ {
		match := true
		for j := 0; j < m; j++ {
			a, b := s[i+j], sub[j]
			if a >= 'a' && a <= 'z' {
				a -= 32
			}
			if b >= 'a' && b <= 'z' {
				b -= 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// failedStatusSeq — 档案含 FAILED 终态（runtime.exit 投影）时返回其事件 seq。
func failedStatusSeq(events []*model.Event) int {
	for _, ev := range events {
		if ev.Type == "task.status" && ev.Status == "FAILED" {
			return ev.Seq
		}
	}
	return 0
}
