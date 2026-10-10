package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// handoff.seeded 线格式锁（schema SSOT event.schema.json handoff_seed：
// required [via,brief_ref,tail]，tail 嵌套 {from_seq,to_seq}，
// additionalProperties:false——Go 结构曾平铺漂移，P2-1 修复的形状锁）。
func TestHandoffSeedShapeMatchesSchema(t *testing.T) {
	ev := Event{
		Seq: 7, Ts: "2026-09-20T00:00:00Z", Task: "T-1", Session: "main",
		Pen: Pen{Agent: "dsh"}, Type: "handoff.seeded",
		HandoffSeed: &HandoffSeed{
			Via: "task-card", BriefRef: "blob:abcdef0123456789",
			Tail: HandoffSeedTail{FromSeq: 1, ToSeq: 6},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `"from_seq":1,`) && strings.Contains(s, `"handoff_seed":{"via"`) && !strings.Contains(s, `"tail":{`) {
		t.Fatalf("from_seq 出现在 handoff_seed 顶层（平铺形状）：%s", s)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	seed, _ := top["handoff_seed"].(map[string]any)
	if seed == nil {
		t.Fatalf("缺 handoff_seed: %s", s)
	}
	if seed["via"] != "task-card" || seed["brief_ref"] != "blob:abcdef0123456789" {
		t.Fatalf("via/brief_ref 形状漂移: %s", s)
	}
	tail, _ := seed["tail"].(map[string]any)
	if tail == nil || tail["from_seq"] != float64(1) || tail["to_seq"] != float64(6) {
		t.Fatalf("tail 未嵌套或值漂移（schema required [from_seq,to_seq] 嵌套）: %s", s)
	}
}
