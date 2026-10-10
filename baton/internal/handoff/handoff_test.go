package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baton/internal/project"
)

var fixedNow = time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)

// seedTasklog — project 产出一个真实档案（复用 project 夹具，链路真实性优先）。
func seedTasklog(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	opts := project.Options{
		SSEPath: "../project/testdata/basic.sse.log", OutRoot: root,
		Task: "T-1042", Goal: "修复订单回调重复扣款",
		Constraints: []string{"不动支付网关SDK"},
		WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Now:         fixedNow,
	}
	if _, err := project.Run(opts); err != nil {
		t.Fatal(err)
	}
	return root
}

func readEvents(t *testing.T, root string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("非法行: %v\n%s", err, ln)
		}
		out = append(out, m)
	}
	return out
}

func TestCheckpointAppend(t *testing.T) {
	root := seedTasklog(t)
	seq, skipped, err := Checkpoint(Options{InRoot: root, GitAnchor: "abc123", Now: fixedNow})
	if err != nil || skipped {
		t.Fatalf("首次锚定: err=%v skipped=%v", err, skipped)
	}
	evs := readEvents(t, root)
	if len(evs) != 12 || seq != 12 {
		t.Fatalf("append 后应 12 条（11+1）, got %d (seq=%d)", len(evs), seq)
	}
	last := evs[11]
	if last["type"] != "handoff.checkpointed" || last["task"] != "T-1042" {
		t.Errorf("事件类型/envelope 不符: %v", last)
	}
	hc := last["handoff_checkpoint"].(map[string]any)
	if hc["log_seq"] != float64(11) {
		t.Errorf("log_seq 应=锚定前行数 11, got %v", hc["log_seq"])
	}
	if ws := hc["workspace"].(map[string]any); ws["git_anchor"] != "abc123" {
		t.Errorf("git_anchor 未落: %v", ws)
	}
	if pen := last["pen"].(map[string]any); pen["agent"] != "dsh" {
		t.Errorf("envelope pen 应抄自末条事件: %v", pen)
	}
	if last["ts"] != "2026-09-17T11:00:00Z" {
		t.Errorf("ts 应为 append 时刻: %v", last["ts"])
	}
}

func TestCheckpointIdempotentRetry(t *testing.T) {
	root := seedTasklog(t)
	if _, _, err := Checkpoint(Options{InRoot: root, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	before := len(readEvents(t, root))
	seq, skipped, err := Checkpoint(Options{InRoot: root, Now: fixedNow.Add(time.Minute)})
	if err != nil || !skipped {
		t.Fatalf("同基线重试应幂等跳过: err=%v skipped=%v", err, skipped)
	}
	if after := len(readEvents(t, root)); after != before {
		t.Errorf("重试不得重复 append: %d → %d", before, after)
	}
	_ = seq
}

func TestCheckpointMissingArchive(t *testing.T) {
	if _, _, err := Checkpoint(Options{InRoot: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "events.jsonl") {
		t.Errorf("无档案应报错（预约冻结=无基线可锚，调用方降级）, got %v", err)
	}
}

// 双段语义链路：project → checkpointed → project 重投影（保留尾段+seq 连续）→
// 第二次 checkpoint（Resume 续跑后的新基线）。
func TestReprojectPreservesHandoffTail(t *testing.T) {
	root := seedTasklog(t)
	if _, _, err := Checkpoint(Options{InRoot: root, GitAnchor: "abc1", Now: fixedNow}); err != nil {
		t.Fatal(err)
	}

	// Resume 后续跑：SSE 增长（拷贝夹具+追加一帧新回合），冻结重投影
	grown := filepath.Join(root, "grown.sse.log")
	b, err := os.ReadFile("../project/testdata/basic.sse.log")
	if err != nil {
		t.Fatal(err)
	}
	extra := "event: session.event\n" +
		`data: {"sessionId":"main","event":{"type":"user/message","data":{"content":[{"type":"text","text":"Resume 后新回合"}]}}}` + "\n\n"
	if err := os.WriteFile(grown, append(b, []byte(extra)...), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := project.Options{
		SSEPath: grown, OutRoot: root,
		Task: "T-1042", Goal: "修复订单回调重复扣款",
		Constraints: []string{"不动支付网关SDK"},
		WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Now:         fixedNow,
	}
	if _, err := project.Run(opts); err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, root)
	if len(evs) != 13 { // 12 条 SSE 派生（新增 user/message）+ 1 条历史 handoff
		t.Fatalf("重投影应保留 handoff 尾段: got %d 条", len(evs))
	}
	last := evs[12]
	if last["type"] != "handoff.checkpointed" {
		t.Fatalf("尾段应保留 handoff 事件, got %v", last["type"])
	}
	if last["seq"] != float64(13) {
		t.Errorf("重投影后 seq 应连续重排为 13, got %v", last["seq"])
	}
	hc := last["handoff_checkpoint"].(map[string]any)
	if hc["log_seq"] != float64(11) || hc["workspace"].(map[string]any)["git_anchor"] != "abc1" {
		t.Errorf("历史 handoff 载荷应为原值: %v", hc)
	}
	if last["ts"] != "2026-09-17T11:00:00Z" {
		t.Errorf("历史 ts 应保留: %v", last["ts"])
	}

	// 二次冻结：SSE 增长后的新基线（log_seq=12=SSE 段行数）
	seq, skipped, err := Checkpoint(Options{InRoot: root, GitAnchor: "def2", Now: fixedNow.Add(time.Minute)})
	if err != nil || skipped {
		t.Fatalf("二次锚定: %v", err)
	}
	evs = readEvents(t, root)
	if len(evs) != 14 || seq != 14 {
		t.Fatalf("二次锚定后应 14 条, got %d", len(evs))
	}
	hc2 := evs[13]["handoff_checkpoint"].(map[string]any)
	if hc2["log_seq"] != float64(13) {
		t.Errorf("新基线 log_seq=13, got %v", hc2["log_seq"])
	}
}

// 脏档案防线：handoff 事件后混入非 handoff 事件 → 重投影整段不保留（只保干净尾段）。
func TestReprojectDirtyTailDrops(t *testing.T) {
	root := seedTasklog(t)
	if _, _, err := Checkpoint(Options{InRoot: root, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	// 手工混入脏行（模拟外部写坏）
	path := filepath.Join(root, ".tasklog", "events.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":13,"ts":"2026-09-17T11:01:00Z","task":"T-1042","pen":{"agent":"dsh"},"type":"task.status","status":"RUNNING"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	opts := project.Options{
		SSEPath: "../project/testdata/basic.sse.log", OutRoot: root,
		Task: "T-1042", Goal: "修复订单回调重复扣款", WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Now: fixedNow,
	}
	stats, err := project.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, root)
	if len(evs) != 11 {
		t.Errorf("脏尾段（handoff 后混入非 handoff）应整段不保留, got %d 条", len(evs))
	}
	found := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "尾段不保留") {
			found = true
		}
	}
	if !found {
		t.Errorf("脏档案应记 Warning: %v", stats.Warnings)
	}
}
