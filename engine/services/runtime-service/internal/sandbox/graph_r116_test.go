package sandbox

// R116：回放面守卫测试——running 孤儿恢复、撕裂
// 容忍、重复 declared 幂等、pause 水位捕获。依据: 主仓库 .agent/evidence/
// baton-review-20260920/report.md §五（engine P1/P2 清单）+ 批同型红驱动。

import (
	"bytes"
	"strings"
	"testing"
)

// replayLines — 便捷回放（行→ReplayGraphLog）。
func replayLines(t *testing.T, lines ...string) (*graphState, error) {
	t.Helper()
	return ReplayGraphLog(bytes.NewReader([]byte(strings.Join(lines, "\n") + "\n")))
}

// R116 P1：started-无-result 的 running 孤儿，回放尾必须归回 declared（重派面）。
// 旧代码：status 卡 stRunning，nextOpen/counts 均不可见 → resume 吞节点假收敛。
func TestReplay_RunningOrphanRecoveredToDeclared(t *testing.T) {
	st, err := replayLines(t,
		`{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`,
		`{"type":"graph.node_started","seq":2,"time":"2026-09-20T10:00:01Z","data":{"nodeId":"n001","sessionId":"g-n001","turnRef":"g-n001"}}`,
		// 崩溃：node_result 缺失
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	n := st.nodes["n001"]
	if n == nil || n.Status != stDeclared {
		t.Fatalf("running 孤儿未归回 declared: %+v", n)
	}
	if n.Attempts != 1 {
		t.Fatalf("crash 恢复应计一次 attempt（crash-loop 封顶执法）: %d", n.Attempts)
	}
	if len(st.recoveredNodes) != 1 || st.recoveredNodes[0] != "n001" {
		t.Fatalf("恢复清单失真: %v", st.recoveredNodes)
	}
	if got := st.nextOpen(); got == nil || got.ID != "n001" {
		t.Fatalf("孤儿应可重派（nextOpen）: %v", got)
	}
	_, _, _, _, open := st.counts()
	if open != 1 {
		t.Fatalf("open 计数应含恢复节点: open=%d", open)
	}
}

// R116 P2：撕裂尾行（JSON 截断）容忍丢弃+计数，不再 fatal 打瘫 resume。
func TestReplay_TornTailTolerated(t *testing.T) {
	st, err := replayLines(t,
		`{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`,
		`{"type":"graph.node_result","seq":2,"time":"2026-09-20T10:00:02Z","data":{"nodeId":"n001","outcome":"completed","resultRef":"ok"}`,
		// ↑ 撕裂行（缺收尾大括号）
	)
	if err != nil {
		t.Fatalf("撕裂尾行应容忍而非 fatal: %v", err)
	}
	if st.droppedLines != 1 {
		t.Fatalf("撕裂行计数: %d", st.droppedLines)
	}
	if st.nodes["n001"].Status != stDeclared {
		t.Fatalf("撕裂的 result 事件应被丢弃（节点仍 declared 待重派）: %+v", st.nodes["n001"])
	}
	// 语义错误仍 fatal：未声明先引用
	_, err = replayLines(t,
		`{"type":"graph.node_result","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"nx","outcome":"completed","resultRef":""}}`)
	if err == nil || !strings.Contains(err.Error(), "未声明先引用") {
		t.Fatalf("语义折叠错误应保持 fatal: %v", err)
	}
}

// R116 顺手批：重复 declared 幂等忽略（污染档容忍）；终态再 declared 仍 fatal。
func TestReplay_DuplicateDeclaredIdempotent(t *testing.T) {
	decl := `{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`
	dup := `{"type":"graph.node_declared","seq":2,"time":"2026-09-20T10:00:01Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`
	st, err := replayLines(t, decl, dup)
	if err != nil {
		t.Fatalf("重复 declared 应幂等容忍: %v", err)
	}
	if len(st.order) != 1 {
		t.Fatalf("order 重复 id: %v", st.order)
	}
	// 终态再 declared = 语义冲突，仍 fatal
	_, err = replayLines(t, decl,
		`{"type":"graph.node_result","seq":2,"time":"2026-09-20T10:00:02Z","data":{"nodeId":"n001","outcome":"completed","resultRef":"ok"}}`,
		dup)
	if err == nil || !strings.Contains(err.Error(), "已终态再 declared") {
		t.Fatalf("终态再 declared 应 fatal: %v", err)
	}
}

// R116 P2：evPaused 捕获已消费新态势水位（resume 恢复 plannerMark 用）。
func TestReplay_PausedWatermarkCaptured(t *testing.T) {
	st, err := replayLines(t,
		`{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`,
		`{"type":"graph.node_result","seq":2,"time":"2026-09-20T10:00:01Z","data":{"nodeId":"n001","outcome":"completed","resultRef":"ok"}}`,
		`{"type":"graph.plan_triggered","seq":3,"time":"2026-09-20T10:00:02Z","data":{"trigger":"new_evidence","detail":"x"}}`,
		`{"type":"graph.paused","seq":4,"time":"2026-09-20T10:00:03Z","data":{"reason":"task_pause","openNodeIds":[],"plannerMark":1}}`,
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !st.hasPausedMark || st.pausedMark != 1 {
		t.Fatalf("pause 水位未捕获: has=%v mark=%d", st.hasPausedMark, st.pausedMark)
	}
}
