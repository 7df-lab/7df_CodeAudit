package project

// Maka 适配器测试（maka-integration.md §3 映射 + README §5 金丝雀纪律）。
// 形状锚点=S2 真实样本（testdata/maka-s2-sample.jsonl，六事件全序列——
// .agent/evidence/maka-s2-20260919/ 归档副本）；handoff/permission 类为源码
// 形状合成（无真实样本，文档如实标注）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makaProject 用 Maka 输入跑一次完整投影，返回工作区根路径与 stats。
func makaProject(t *testing.T, sample string) (string, *Stats) {
	t.Helper()
	root := t.TempDir()
	opts := Options{
		MakaPath: sample, OutRoot: root,
		Task: "task-maka-1", Goal: "S2 校准目标", Agent: "maka",
	}
	st, err := Run(opts)
	if err != nil {
		t.Fatalf("Run(maka): %v", err)
	}
	return root, st
}

// TestMaka_S2SampleProjection — S2 真实样本全链：六事件→合成帧→投影。
// 验收：user/assistant message 配对、usage 数值映射（cacheHit→cacheRead）、
// 模型名来自 route.modelId（bridge.hello 唯一来源语义）。
func TestMaka_S2SampleProjection(t *testing.T) {
	sample := filepath.Join("testdata", "maka-s2-sample.jsonl")
	root, st := makaProject(t, sample)

	raw, err := os.ReadFile(filepath.Join(root, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(raw)
	for _, want := range []string{
		`"type":"task.created"`,
		`"role":"user"`,
		`"type":"model.message"`,
		`HELLO-FROM-SANDBOX`, // assistant 正文（text kind agent）
		`"type":"usage.recorded"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("投影缺 %s\n%s", want, joined)
		}
	}
	// usage 映射：S2 样本 input=3026（int64 直出数字形态）
	if !strings.Contains(joined, `"input_tokens":3026`) {
		t.Fatalf("usage input 映射失真:\n%s", joined)
	}
	// thinking→reasoning part（assistant message 含 thinking 文本片段）
	if !strings.Contains(joined, `"kind":"reasoning"`) {
		t.Fatalf("thinking 未映射为 reasoning part:\n%s", joined)
	}
	_ = st
}

// TestMaka_UsageModelAttribution — usage.recorded 的 model 取 bridge.hello
// （route.modelId），空 model 降级为空串（schema required 降级语义）。
func TestMaka_UsageModelAttribution(t *testing.T) {
	sample := filepath.Join("testdata", "maka-s2-sample.jsonl")
	root, _ := makaProject(t, sample)
	raw, err := os.ReadFile(filepath.Join(root, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model":"glm-5.3-flash"`) {
		t.Fatalf("usage model 未取 route.modelId:\n%s", raw)
	}
}

// TestMaka_ParkedCanary — schema 漂移行：非 JSON/白名单外 kind→PARKED 落档+
// stats.Parked 计数；好行投影不受影响（丢行=丢档案，投影继续）。
func TestMaka_ParkedCanary(t *testing.T) {
	sample := filepath.Join("testdata", "maka-s2-sample.jsonl")
	raw, _ := os.ReadFile(sample)
	broken := append([]byte{}, raw...)
	broken = append(broken, []byte("{not-json\n")...)
	broken = append(broken, []byte(`{"event_id":"x","event_kind":"mystery_kind","payload_json":"{}"}`+"\n")...)

	dir := t.TempDir()
	p := filepath.Join(dir, "broken.jsonl")
	if err := os.WriteFile(p, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	root, st := makaProject(t, p)
	if st.Parked != 2 {
		t.Fatalf("Parked = %d, want 2", st.Parked)
	}
	pk, err := os.ReadFile(filepath.Join(root, ".tasklog", "PARKED"))
	if err != nil {
		t.Fatalf("PARKED 未落档: %v", err)
	}
	if !strings.Contains(string(pk), "白名单外") || !strings.Contains(string(pk), "非 JSON") {
		t.Fatalf("PARKED 原因失真: %s", pk)
	}
	// 好行仍投影：HELLO-FROM-SANDBOX 在档
	ev, _ := os.ReadFile(filepath.Join(root, ".tasklog", "events.jsonl"))
	if !bytes.Contains(ev, []byte("HELLO-FROM-SANDBOX")) {
		t.Fatal("park 后好行投影被误杀")
	}
}

// TestMaka_ContinuationWarning — 多回合任务第二 invocation source.kind=
// continuation → warning 留痕（源高位水指针；投影形状 v1 定档）。
// handoff 子类同路径（源码形状合成——真实样本待跨 harness 换手）。
func TestMaka_ContinuationWarning(t *testing.T) {
	sample := filepath.Join("testdata", "maka-s2-sample.jsonl")
	raw, _ := os.ReadFile(sample)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// 克隆首行（invocation_opened fresh）为 continuation 新回合。payload_json
	// 是转义内嵌字符串，文件原文形态为 \"kind\": \"fresh\"。
	first := strings.Replace(lines[0], `\"kind\":\"fresh\"`, `\"kind\":\"continuation\"`, 1)
	if first == lines[0] {
		first = strings.Replace(lines[0], `"fresh"`, `"continuation"`, 1)
	}
	if first == lines[0] {
		t.Fatal("样本无 fresh source——结构不符")
	}
	joined := strings.Join(append(lines, first), "\n") + "\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "cont.jsonl")
	if err := os.WriteFile(p, []byte(joined), 0o644); err != nil {
		t.Fatal(err)
	}
	_, st := makaProject(t, p)
	found := false
	for _, w := range st.Warnings {
		if strings.Contains(w, "continuation") {
			found = true
		}
	}
	if !found {
		t.Fatalf("continuation 未留痕: %v", st.Warnings)
	}
}

// TestMaka_LiveToolRound — sim 真实工具回合样本（testdata/maka-live-sample.jsonl，
// 20 事件含 function_call/tool_dispatch/function_response）：tool.call/tool.result
// 按 callId 配对、args 对象原值入库、modelProjection 文本落 result。
func TestMaka_LiveToolRound(t *testing.T) {
	sample := filepath.Join("testdata", "maka-live-sample.jsonl")
	root, st := makaProject(t, sample)
	raw, err := os.ReadFile(filepath.Join(root, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(raw)
	n := strings.Count(joined, `"type":"tool.call"`)
	if n < 4 {
		t.Fatalf("tool.call 数=%d, want >=4（Glob/Read/Bash 真实调用）\n%s", n, joined[:400])
	}
	if !strings.Contains(joined, `"type":"tool.result"`) {
		t.Fatal("tool.result 缺失——配对失败")
	}
	if !strings.Contains(joined, `"tool":"Glob"`) {
		t.Fatal("tool 名失真")
	}
	if st.Parked != 0 {
		t.Fatalf("真实样本不应 park（Parked=%d）——白名单/形状失配", st.Parked)
	}
}

// TestMaka_TranspileDeterministic — 同输入两次转译逐字节一致（确定性=全量重算
// 幂等的前提，黄金纪律同 DSH 投影器）。
func TestMaka_TranspileDeterministic(t *testing.T) {
	sample := filepath.Join("testdata", "maka-s2-sample.jsonl")
	a, _, err := transpileMaka(sample)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := transpileMaka(sample)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("转译不确定：同输入两次输出不一致")
	}
}
