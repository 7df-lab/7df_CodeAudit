package project

// R122：机械提炼失败要点 → spine.facts。
// 锁：lesson 四模板（失败模式/错误输出/REFUSED/FAILED 终态）、去重、上限保最新、
// 贫 spine 纪律（干净流零 facts——observation 不自动提炼，brief_test 同款红线）。

import (
	"encoding/json"
	"strings"
	"testing"

	"baton/internal/model"
)

func errResult(seq int, sid, callID, msg string) *model.Event {
	return &model.Event{Seq: seq, Session: sid, Type: "tool.result",
		Result: &model.Result{CallID: callID, Outcome: "error", Ok: false,
			Result: mustQuoted(msg)}}
}

func indetResult(seq int, sid, callID string) *model.Event {
	return &model.Event{Seq: seq, Session: sid, Type: "tool.result",
		Result: &model.Result{CallID: callID, Outcome: "indeterminate"}}
}

func failedStatus(seq int) *model.Event {
	return &model.Event{Seq: seq, Type: "task.status", Status: "FAILED"}
}

func mustQuoted(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

func TestExtractLessons_Templates(t *testing.T) {
	evs := []*model.Event{
		{Seq: 1, Session: "g-n003", Type: "tool.result",
			Result: &model.Result{CallID: "call_9", Outcome: "error",
				Result: mustQuoted("REFUSED: policy denies write to /etc")}},
		indetResult(2, "g-n003", "call_10"),
		failedStatus(3),
	}
	facts := extractLessons(evs)
	joined := ""
	for _, f := range facts {
		if f.Kind != "lesson" {
			t.Fatalf("全部应为 lesson: %+v", f)
		}
		if f.Session != "g-n003" && !strings.HasPrefix(f.Text, "任务曾以") {
			t.Fatalf("session 维度缺失（任务级 FAILED 事实除外）: %+v", f)
		}
		if len(f.Evidence) == 0 {
			t.Fatalf("lesson 必须带 evidence seq: %+v", f)
		}
		joined += f.Text + "\n"
	}
	for _, want := range []string{"错误输出：REFUSED", "工具拒绝执行（REFUSED）", "结果不明", "任务曾以 FAILED"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺模板要点 %q\n%s", want, joined)
		}
	}
	for _, f := range facts {
		if len(f.Evidence) == 0 {
			t.Fatalf("lesson 必须带 evidence seq: %+v", f)
		}
	}
}

func TestExtractLessons_DedupAndCap(t *testing.T) {
	var evs []*model.Event
	for i := 1; i <= 30; i++ {
		evs = append(evs, errResult(i, "g-x", "call_"+itoa(i), "boom "+itoa(i)))
	}
	facts := extractLessons(evs)
	if len(facts) != lessonCap {
		t.Fatalf("上限保最新: %d != %d", len(facts), lessonCap)
	}
	// 保最新：最旧（boom 1）不在，最新（boom 30）在
	for _, f := range facts {
		if strings.Contains(f.Text, "boom 1\n") || strings.HasSuffix(f.Text, "boom 1") {
			t.Fatal("最旧事实应被裁掉")
		}
	}
	if !strings.Contains(facts[len(facts)-1].Text, "boom 30") {
		t.Fatalf("最新事实应保尾: %s", facts[len(facts)-1].Text)
	}
	// 去重：同 session+text 只一条
	dup := []*model.Event{errResult(1, "g-d", "c1", "same"), errResult(2, "g-d", "c2", "same")}
	if got := len(extractLessons(dup)); got != 1 {
		t.Fatalf("去重失效: %d", got)
	}
}

// 贫 spine 纪律：干净流（全 completed、无 FAILED）零 facts（brief_test 红线对齐）。
func TestExtractLessons_CleanFlowZeroFacts(t *testing.T) {
	evs := []*model.Event{
		{Seq: 1, Session: "main", Type: "tool.result",
			Result: &model.Result{CallID: "c1", Outcome: "completed", Ok: true}},
	}
	if got := extractLessons(evs); len(got) != 0 {
		t.Fatalf("干净流应零 facts: %v", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
