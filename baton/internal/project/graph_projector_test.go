package project

// graph.* 双输入投影测试（切片 5+6 ADR-001）：-graph 第二输入 + ts 归并（同刻
// graph-first）+ spine open 折叠。依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §6。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func graphBaseOpts(t *testing.T, sse string) Options {
	t.Helper()
	return baseOpts(t, sse)
}

// 双输入归并：六类型入档、ts 序（同刻 graph 先）、spine open 折叠、session 归属。
func TestGraphMerge_DualInput(t *testing.T) {
	out := t.TempDir()
	stats, err := Run(Options{
		SSEPath:   filepath.Join("testdata", "graph_basic.sse.log"),
		GraphPath: filepath.Join("testdata", "graph_basic.graph.log"),
		OutRoot:   out, Task: "T-G1", Goal: "图任务目标",
		Agent: "dsh", WorkspaceID: fixedWS, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// 六类型全部入档
	for _, typ := range []string{"graph.node_declared", "graph.node_started", "graph.node_result",
		"graph.plan_triggered", "graph.converged"} {
		if stats.ByType[typ] == 0 {
			t.Fatalf("type %s missing (ByType=%v)", typ, stats.ByType)
		}
	}
	raw, err := os.ReadFile(filepath.Join(out, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var evs []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("非法行: %v", err)
		}
		evs = append(evs, ev)
	}
	idx := func(pred func(map[string]any) bool) int {
		for i, ev := range evs {
			if pred(ev) {
				return i
			}
		}
		return -1
	}
	// ts 归并：node_declared(10:00:00) 在 main user/message(10:00:01) 之后？
	// hello 帧无 ts（降级）→ 直接投影；declared(00:00) 插在 01 帧前——顺序=hello,
	// declared, user/message(01)…
	iHello := idx(func(e map[string]any) bool { return e["type"] == "model.message" && graphStr(e, "session") == "main" })
	iDeclared := idx(func(e map[string]any) bool { return e["type"] == "graph.node_declared" })
	if iDeclared < 0 || iHello < 0 || iDeclared > iHello {
		t.Fatalf("declared(10:00:00) must precede main message(10:00:01): declared@%d message@%d", iDeclared, iHello)
	}
	// 同刻 graph-first：node_started(10:00:02) 先于 g-n001 的 user/message(10:00:02)
	iStarted := idx(func(e map[string]any) bool { return e["type"] == "graph.node_started" })
	iNodeMsg := idx(func(e map[string]any) bool { return e["type"] == "model.message" && graphStr(e, "session") == "g-n001" })
	if iStarted < 0 || iNodeMsg < 0 || iStarted > iNodeMsg {
		t.Fatalf("node_started must precede same-ts session frame (graph-first): started@%d msg@%d", iStarted, iNodeMsg)
	}
	// session 归属：node_started 带 sessionId；node_result 推导 g-<nodeId>
	if s := sessionOf(evs, "graph.node_started"); s != "g-n001" {
		t.Fatalf("node_started session = %q, want g-n001", s)
	}
	if s := sessionOf(evs, "graph.node_result"); s != "g-n001" {
		t.Fatalf("node_result session = %q, want g-n001（g-<nodeId> 推导）", s)
	}
	// 域中立：载荷无业务词（severity 等只可能在 resultRef 文本内——此处夹具无）
	for _, ev := range evs {
		if strings.HasPrefix(graphStr(ev, "type"), "graph.") {
			b, _ := json.Marshal(ev)
			if strings.Contains(string(b), "\"severity\"") || strings.Contains(string(b), "\"findings\"") {
				t.Fatalf("graph event carries business schema words (C4): %s", b)
			}
		}
	}
	// 最终 spine：open 折叠至空（n001/n002 均终态）
	spineRaw, err := os.ReadFile(filepath.Join(out, ".tasklog", "spine.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spine struct {
		Open []string `json:"open"`
	}
	if err := json.Unmarshal(spineRaw, &spine); err != nil {
		t.Fatal(err)
	}
	if len(spine.Open) != 0 {
		t.Fatalf("final open = %v, want empty（completed 出 open）", spine.Open)
	}
	// 中段 spine 快照含 open（n001 declared 后 open=[n001]）
	sawOpen := false
	for _, ev := range evs {
		if ev["type"] == "spine.updated" {
			if sp, ok := ev["spine"].(map[string]any); ok {
				if open, ok := sp["open"].([]any); ok && len(open) == 1 && open[0] == "n001" {
					sawOpen = true
				}
			}
		}
	}
	if !sawOpen {
		t.Fatal("mid-stream spine snapshot must carry open=[n001] after declared")
	}
}

// indeterminate 留 open（D1：没跑完≠终态——与 engine 折叠规则跨仓一致）。
func TestGraphMerge_IndeterminateStaysOpen(t *testing.T) {
	dir := t.TempDir()
	sse := ": ts 2026-09-19T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	if err := os.WriteFile(filepath.Join(dir, "in.sse.log"), []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	gl := `{"type":"graph.node_declared","seq":1,"time":"2026-09-19T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}
{"type":"graph.node_started","seq":2,"time":"2026-09-19T10:00:02Z","data":{"nodeId":"n001","sessionId":"g-n001","turnRef":"g-n001"}}
{"type":"graph.node_result","seq":3,"time":"2026-09-19T10:00:03Z","data":{"nodeId":"n001","outcome":"indeterminate","resultRef":"超时"}}
`
	if err := os.WriteFile(filepath.Join(dir, "in.graph.log"), []byte(gl), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if _, err := Run(Options{
		SSEPath: filepath.Join(dir, "in.sse.log"), GraphPath: filepath.Join(dir, "in.graph.log"),
		OutRoot: out, Task: "T-G2", Goal: "g", Agent: "dsh", WorkspaceID: fixedWS, Now: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	spineRaw, _ := os.ReadFile(filepath.Join(out, ".tasklog", "spine.json"))
	if !strings.Contains(string(spineRaw), `"n001"`) {
		t.Fatalf("indeterminate must stay in open: %s", spineRaw)
	}
}

// 向后兼容：-graph 空路径（或文件不存在）行为与既有单输入逐字节一致。
func TestGraphMerge_AbsentGraphIsNoop(t *testing.T) {
	out1, out2 := t.TempDir(), t.TempDir()
	o := graphBaseOpts(t, filepath.Join("testdata", "basic.sse.log"))
	o.OutRoot, o.WorkspaceID, o.Now = out1, fixedWS, fixedNow
	if _, err := Run(o); err != nil {
		t.Fatal(err)
	}
	o2 := o
	o2.OutRoot = out2
	o2.GraphPath = filepath.Join("testdata", "no-such.graph.log") // 不存在=非图任务
	if _, err := Run(o2); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(out1, ".tasklog", "events.jsonl"))
	b, _ := os.ReadFile(filepath.Join(out2, ".tasklog", "events.jsonl"))
	if string(a) != string(b) {
		t.Fatalf("absent graph input must be byte-identical to legacy projection")
	}
}

// 未知 graph.* 类型：忽略不炸（前向兼容，schema 演进期兜底）。
func TestGraphMerge_UnknownTypeIgnored(t *testing.T) {
	dir := t.TempDir()
	sse := ": ts 2026-09-19T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	os.WriteFile(filepath.Join(dir, "in.sse.log"), []byte(sse), 0o644)
	gl := `{"type":"graph.future_happens","seq":1,"time":"2026-09-19T10:00:00Z","data":{"x":1}}
{"type":"graph.converged","seq":2,"time":"2026-09-19T10:00:09Z","data":{"summary":"ok","openRemaining":0}}
`
	os.WriteFile(filepath.Join(dir, "in.graph.log"), []byte(gl), 0o644)
	out := t.TempDir()
	stats, err := Run(Options{
		SSEPath: filepath.Join(dir, "in.sse.log"), GraphPath: filepath.Join(dir, "in.graph.log"),
		OutRoot: out, Task: "T-G3", Goal: "g", Agent: "dsh", WorkspaceID: fixedWS, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ByType["graph.future_happens"] != 0 {
		t.Fatalf("unknown graph type must be ignored, ByType=%v", stats.ByType)
	}
	if stats.ByType["graph.converged"] != 1 {
		t.Fatalf("known type must project")
	}
}

// graph 事件 ts 用事件发生时刻（非投影时刻）。
func TestGraphMerge_EventTimeUsed(t *testing.T) {
	out := t.TempDir()
	if _, err := Run(Options{
		SSEPath:   filepath.Join("testdata", "graph_basic.sse.log"),
		GraphPath: filepath.Join("testdata", "graph_basic.graph.log"),
		OutRoot:   out, Task: "T-G4", Goal: "g", Agent: "dsh", WorkspaceID: fixedWS,
		Now: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(out, ".tasklog", "events.jsonl"))
	if !strings.Contains(string(raw), "2026-09-19T10:00:00Z") {
		t.Fatalf("graph event ts must be event time, not projection time (2099)")
	}
}

func graphStr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func sessionOf(evs []map[string]any, typ string) string {
	for _, ev := range evs {
		if ev["type"] == typ {
			return graphStr(ev, "session")
		}
	}
	return ""
}
