package sandbox

// graph 发射器金样本：真实 graphEmitter 输出的 JSONL 逐字节锁定
// （testdata/graph_emit_golden.jsonl）——锚定**本发射器自身**的确定性（同输入
// 同字节；时间字段固定 now 注入）。baton 仓 internal/project/testdata/
// engine_emitted.graph.log 曾为同字节副本，2026-09-20 人类质疑修正：两子仓功能
// 不同，逐字节同步义务废除——baton 副本降级为"某时点形状快照"（其消费面测试
// 改语义兼容断言）；本测仅锁 engine 自己不漂。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGraphEmit_GoldenStable(t *testing.T) {
	fixed := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	e := newGraphEmitter(0, func(b []byte) { out.Write(b) })
	e.now = func() time.Time { return fixed }

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	must(e.emit("graph.node_declared", map[string]any{
		"nodeId": "n001", "type": "executor", "intent": "初扫注入面：SQL 注入与硬编码凭据",
		"declaredBy": "pack", "from": []string{}}))
	must(e.emit("graph.node_started", map[string]any{
		"nodeId": "n001", "sessionId": "g-n001", "turnRef": "g-n001"}))
	must(e.emit("graph.node_result", map[string]any{
		"nodeId": "n001", "outcome": "completed", "resultRef": "3 处发现（B608×2/B105）",
		"usageEstimate": map[string]any{"turns": float64(3)}}))
	must(e.emit("graph.plan_triggered", map[string]any{
		"trigger": "new_evidence", "detail": "n001 报告可疑残面，重规划复核节点"}))
	must(e.emit("graph.paused", map[string]any{
		"reason": "handoff", "openNodeIds": []string{"n002"}, "plannerMark": 1}))
	must(e.emit("graph.converged", map[string]any{
		"summary": "open 排空且无新态势：completed=1 failed=0 rejected=0", "openRemaining": 0}))

	golden, err := os.ReadFile(filepath.Join("testdata", "graph_emit_golden.jsonl"))
	if err != nil {
		// 首跑落盘（BATON_UPDATE_GOLDEN 同型：显式 env 防误更新）
		if os.Getenv("GRAPH_EMIT_UPDATE_GOLDEN") == "1" {
			if werr := os.WriteFile(filepath.Join("testdata", "graph_emit_golden.jsonl"), out.Bytes(), 0o644); werr != nil {
				t.Fatalf("写金样本: %v", werr)
			}
			t.Log("金样本已落盘（testdata/graph_emit_golden.jsonl）")
			return
		}
		t.Fatalf("金样本缺失：GRAPH_EMIT_UPDATE_GOLDEN=1 go test 生成后同步 baton 副本: %v", err)
	}
	if !bytes.Equal(out.Bytes(), golden) {
		t.Fatalf("发射器输出与金样本漂移（跨仓契约变更：同步 baton internal/project/testdata/engine_emitted.graph.log 并跑其消费面测试）\n--- got ---\n%s\n--- want ---\n%s", out.String(), golden)
	}
}
