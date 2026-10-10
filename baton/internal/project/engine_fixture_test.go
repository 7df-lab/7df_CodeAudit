package project

// 跨仓联测：本文件=engine graphEmitter
// 某时点发射形状快照（生成自 engine TestGraphEmit_GoldenStable），用于消费面
// 语义兼容回归——**非逐字节契约**：engine 输出格式演进（字段增删/时间格式）无需
// 同步本副本（本消费面容错：未知类型忽略/字段缺失容忍）；仅破坏性变更（信封
// 形状/事件名/必需字段语义）才更新副本并跑本测。engine 侧逐字节锁只锚定其
// 自身发射确定性，与本副本无同步义务（两子仓功能不同）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineEmittedFixtureConsumed(t *testing.T) {
	dir := t.TempDir()
	sse := ": ts 2026-09-20T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	ssePath := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(ssePath, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{
		SSEPath:   ssePath,
		GraphPath: filepath.Join("testdata", "engine_emitted.graph.log"),
		OutRoot:   filepath.Join(dir, "out"), Task: "T-XREPO", Goal: "g",
		Agent: "dsh", WorkspaceID: fixedWS, Now: fixedNow,
	}
	stats, err := Run(o)
	if err != nil {
		t.Fatalf("engine 金样本投影失败: %v", err)
	}
	for _, typ := range []string{"graph.node_declared", "graph.node_started", "graph.node_result",
		"graph.plan_triggered", "graph.paused", "graph.converged"} {
		if stats.ByType[typ] != 1 {
			t.Fatalf("engine 事件 %s 解析缺失（ByType=%v）", typ, stats.ByType)
		}
	}
	for _, w := range stats.Warnings {
		if strings.Contains(w, "graph.log 非法行") || strings.Contains(w, "graph.log 扫描失败") {
			t.Fatalf("快照样本不应产生解析告警（语义兼容面破）: %v", stats.Warnings)
		}
	}
	// spine open 折叠：n001 completed 出 open → 最终 open 空
	spine := readTasklog(t, o.OutRoot, "spine.json")
	if strings.Contains(spine, `"open": ["n001"`) || strings.Contains(spine, `"open":["n001"`) {
		t.Fatalf("n001 已终态不应留 open: %s", spine)
	}
}
