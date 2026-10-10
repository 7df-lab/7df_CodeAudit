package pack

// execution:graph 字段族测试（切片 5+6 TP18-T1，ADR-249）：
//   - 向后兼容：缺省 execution=session（现网 pack 零变化）；
//   - graph 段校验：枚举/零值矛盾/负值拒载（各给字段路径）；
//   - revision 语义：execution 字段入 canonical 哈希（同 spec 同哈希、异 spec 异哈希）；
//   - Lookup：name@revision 形态重读（ADR-249 快照通路）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §5（schema 增量，向后兼容）。

import (
	"strings"
	"testing"
)

func graphYAML(exec string, graphBlock string) []byte {
	var sb strings.Builder
	sb.WriteString(`apiVersion: codeaudit.pack/v0
kind: WorkloadPack
name: graph-sample
description: 图执行负载样例
spec:
  image: dsh-pentest-sse:latest
  dialect: dsh-sse
  network:
    profile: targeted
  seeds:
    brief_renderer: baton-brief
    render_target: task-card-prefix
    template_ref: assignmentTemplate@frozen
  results:
    schema_ref: findings/submit_findings@v1
    parser: codeaudit-submit
  timeouts:
    per_turn: default
    task_total: audit-default
  constraints_schema: [severity_threshold]
`)
	if exec != "" {
		sb.WriteString("  execution: " + exec + "\n")
	}
	sb.WriteString(graphBlock)
	return []byte(sb.String())
}

const graphFullBlock = `  graph:
    seed_intents: ["侦察目标端口面", "凭据复用试探"]
    constraints:
      max_nodes: 24
      allowed_types: [planner, executor, verifier]
      max_planner_replans: 5
      max_attempts_per_node: 2
    timeouts:
      planner: 180
      executor: 900
      verifier: 300
`

// 向后兼容三面：缺省 session（无 execution 键）/显式 session/合法 graph 全量。
func TestExecution_BackwardCompatible(t *testing.T) {
	if _, err := Parse(graphYAML("", ""), nil); err != nil {
		t.Fatalf("default execution (no key) must pass, got: %v", err)
	}
	if _, err := Parse(graphYAML("session", ""), nil); err != nil {
		t.Fatalf("explicit session must pass, got: %v", err)
	}
	p, err := Parse(graphYAML("graph", graphFullBlock), nil)
	if err != nil {
		t.Fatalf("graph pack rejected: %v", err)
	}
	if p.Spec.Execution != "graph" || len(p.Spec.Graph.SeedIntents) != 2 {
		t.Fatalf("graph fields not parsed: %+v", p.Spec.Graph)
	}
	if p.Spec.Graph.Constraints.MaxNodes != 24 || p.Spec.Graph.Timeouts.Executor != 900 {
		t.Fatalf("constraints/timeouts not parsed: %+v", p.Spec.Graph)
	}
}

// execution=session 携带非零 graph 段=形态矛盾拒载（防部分应用，D1）。
func TestExecution_SessionWithGraphRejected(t *testing.T) {
	_, err := Parse(graphYAML("session", graphFullBlock), nil)
	if err == nil || !strings.Contains(err.Error(), "spec.graph") {
		t.Fatalf("session+graph must reject with field path, got: %v", err)
	}
	// 缺省（无 execution 键）同理——graph 段出现即矛盾
	_, err = Parse(graphYAML("", graphFullBlock), nil)
	if err == nil || !strings.Contains(err.Error(), "spec.graph") {
		t.Fatalf("default+graph must reject with field path, got: %v", err)
	}
}

// graph 段字段级拒载：非法枚举（execution/allowed_types）、空 seed、负值。
func TestExecution_GraphFieldRejects(t *testing.T) {
	cases := []struct {
		exec, block, field string
	}{
		{"pipeline", "", "spec.execution"},
		{"graph", "  graph:\n    constraints:\n      allowed_types: [planner, worker]\n", "allowed_types"},
		{"graph", "  graph:\n    seed_intents: [\"\", \"ok\"]\n", "seed_intents[0]"},
		{"graph", "  graph:\n    constraints:\n      max_nodes: -1\n", "max_nodes"},
		{"graph", "  graph:\n    constraints:\n      max_attempts_per_node: -2\n", "max_attempts_per_node"},
		{"graph", "  graph:\n    timeouts:\n      planner: -180\n", "timeouts"},
	}
	for _, c := range cases {
		_, err := Parse(graphYAML(c.exec, c.block), nil)
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Fatalf("case exec=%q must reject at %q, got: %v", c.exec, c.field, err)
		}
	}
}

// execution/graph 入 canonical 哈希：异 spec 异哈希（revision 复用判定正确性）。
func TestExecution_SpecHashDiscriminates(t *testing.T) {
	a, err := Parse(graphYAML("", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(graphYAML("graph", graphFullBlock), nil)
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := a.SpecHash()
	hb, _ := b.SpecHash()
	if ha == hb {
		t.Fatalf("session vs graph specs must hash differently: %s", ha)
	}
	// 同输入同哈希（幂等）
	b2, _ := Parse(graphYAML("graph", graphFullBlock), nil)
	hb2, _ := b2.SpecHash()
	if hb != hb2 {
		t.Fatalf("graph spec hash unstable: %s vs %s", hb, hb2)
	}
}

// Registry.Lookup — name@revision 快照重读（ADR-249：任务启动时 GraphSpec 填充通路）。
func TestRegistry_Lookup(t *testing.T) {
	reg := NewRegistry()
	rev1, _, err := reg.Register(mustParse(t, graphYAML("graph", "  graph:\n    seed_intents: [\"v1\"]\n")), "test")
	if err != nil {
		t.Fatal(err)
	}
	rev2, _, err := reg.Register(mustParse(t, graphYAML("graph", "  graph:\n    seed_intents: [\"v2\"]\n")), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reg.Lookup(rev1.ID()); err != nil || got != rev1 {
		t.Fatalf("lookup rev1: %v", err)
	}
	if got, err := reg.Lookup(rev2.ID()); err != nil || got != rev2 {
		t.Fatalf("lookup rev2: %v", err)
	}
	for _, bad := range []string{"no-at", "graph-sample@", "@1", "unknown@9", "graph-sample@abc"} {
		if _, err := reg.Lookup(bad); err == nil {
			t.Fatalf("lookup %q must fail", bad)
		}
	}
}

func mustParse(t *testing.T, data []byte) *Pack {
	t.Helper()
	p, err := Parse(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
