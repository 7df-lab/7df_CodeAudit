package service

// GetTaskGraph 回折投影测试（切片5+6 暴露批 P1a）：fixture=engine 发射器金样本
// 同源字节（TODO#5 跨仓联测）；坏行容忍/空态/折叠规则。

import (
	"os"
	"path/filepath"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetTaskGraph_FoldsEngineFixture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEAUDIT_INTERACTION_DIR", dir)
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "baton", "internal", "project", "testdata", "engine_emitted.graph.log"))
	if err != nil {
		t.Skipf("baton 金样本副本不可达（跨仓检出布局差异）: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(dir, "gw-g1.graph.log"), raw, 0o644); werr != nil {
		t.Fatal(werr)
	}
	s := newSvc(t)
	g, err := s.GetTaskGraph(t.Context(), &pb.GetTaskGraphRequest{TaskId: "gw-g1"})
	if err != nil {
		t.Fatalf("GetTaskGraph: %v", err)
	}
	if len(g.GetNodes()) != 1 || g.GetNodes()[0].GetId() != "n001" {
		t.Fatalf("节点投影失真: %v", g.GetNodes())
	}
	if seq := g.GetNodes()[0].GetDeclaredSeq(); seq != 1 {
		t.Fatalf("declared_seq 批次归属键失真: %d", seq)
	}
	n := g.GetNodes()[0]
	if n.GetStatus() != "completed" || n.GetIntent() == "" || n.GetResultRef() == "" {
		t.Fatalf("节点字段保真失真: %+v", n)
	}
	if len(g.GetPlanTriggers()) != 1 || g.GetPlanTriggers()[0].GetTrigger() != "new_evidence" || g.GetPlanTriggers()[0].GetSeq() != 4 {
		t.Fatalf("计划触发史失真: %v", g.GetPlanTriggers())
	}
	if !g.GetPaused() || g.GetPauseReason() != "handoff" {
		t.Fatalf("挂起态失真: paused=%v reason=%q", g.GetPaused(), g.GetPauseReason())
	}
	if !g.GetConverged() || g.GetConvergeSummary() == "" {
		t.Fatalf("收敛态失真: %+v", g)
	}
	if g.GetOpenRemaining() != 0 {
		t.Fatalf("open 剩余应 0: %d", g.GetOpenRemaining())
	}
}

func TestGetTaskGraph_EmptyHonestState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEAUDIT_INTERACTION_DIR", dir)
	s := newSvc(t)
	g, err := s.GetTaskGraph(t.Context(), &pb.GetTaskGraphRequest{TaskId: "gw-none"})
	if err != nil {
		t.Fatalf("非图任务应空态而非错误: %v", err)
	}
	if len(g.GetNodes()) != 0 {
		t.Fatalf("非图任务应空 nodes: %v", g.GetNodes())
	}
}

// R123：task_id 进共享卷文件名——路径消毒（R53 同款白名单）。穿越面在内部
// gRPC 口同样关门（result-service ExportFindings 同面先例）。
func TestGetTaskGraph_RejectsIllegalTaskID(t *testing.T) {
	s := newSvc(t)
	for _, bad := range []string{"../evil", "a/b", "a..b", "sp ace", "../../etc/passwd#x"} {
		g, err := s.GetTaskGraph(t.Context(), &pb.GetTaskGraphRequest{TaskId: bad})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("非法 task_id %q 应 InvalidArgument，got %v（resp=%+v）", bad, err, g)
		}
	}
	if g, err := s.GetTaskGraph(t.Context(), &pb.GetTaskGraphRequest{TaskId: "gw-ok.1_2"}); err != nil {
		t.Fatalf("合法字符集 task_id 不应拒: %v", err)
	} else if g == nil {
		t.Fatal("合法 task_id 应有响应")
	}
}

// R125：视图折叠对齐权威侧（ReplayGraphLog）——①declared 不预填 started_at；
// ②回放尾 running 孤儿归回 declared（重派面）；③open_node_ids 契约面
// （§1.1 paused 挂起横幅"未决节点 n"数据源）。
func TestFoldGraphLog_AuthorityAligned(t *testing.T) {
	decl := `{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`
	start := `{"type":"graph.node_started","seq":2,"time":"2026-09-20T10:00:01Z","data":{"nodeId":"n001"}}`
	g, warns := foldGraphLog([][]byte{[]byte(decl)})
	if g.GetNodes()[0].GetStartedAt() != "" {
		t.Fatalf("declared 不应预填 started_at（权威侧无此语义）: %q", g.GetNodes()[0].GetStartedAt())
	}
	g, _ = foldGraphLog([][]byte{[]byte(decl), []byte(start)})
	if g.GetNodes()[0].GetStartedAt() == "" {
		t.Fatal("started 后应有起点时刻")
	}
	// 崩溃孤儿：started-无-result → 归回 declared+Attempts++，open 计数/名单可见。
	g, warns = foldGraphLog([][]byte{[]byte(decl), []byte(start)})
	n := g.GetNodes()[0]
	if n.GetStatus() != "declared" || n.GetAttempts() != 1 {
		t.Fatalf("running 孤儿未归回 declared（重派面）: %+v", n)
	}
	if len(warns) == 0 {
		t.Fatal("孤儿归回应留痕（warnings）")
	}
	if g.GetOpenRemaining() != 1 || len(g.GetOpenNodeIds()) != 1 || g.GetOpenNodeIds()[0] != "n001" {
		t.Fatalf("open 契约面失真: remaining=%d ids=%v", g.GetOpenRemaining(), g.GetOpenNodeIds())
	}
}
