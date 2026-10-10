package service

// R138 锁定测试三件（第四轮审计遗留清偿批）：
//  1. A9 分页钳制：阈值与钳入值同源（配置权威双向）——原"阈值字面量 100、钳入
//     取配置"在 max<100 时 (max,100] 绕过钳制、max>100 时反而放大请求。
//  2. P3-3 内存档分页：尾页恰好剩 limit 条时不得产出幻影 next（PG 版 limit+1
//     探测语义）；游标引用已删条目不得归零重放（整页重复）。
//  3. P3-1 CreateFinding 单条路径 ai_reasoning 与 Batch 同口径落盘。

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/codeaudit/services/result-service/internal/repository"
)

func TestClampPageSize(t *testing.T) {
	cases := []struct {
		in, def, max, want int
		what               string
	}{
		{0, 20, 100, 20, "非正取缺省"},
		{-5, 20, 100, 20, "负数取缺省"},
		{50, 20, 100, 50, "区间内不动"},
		{80, 20, 50, 50, "max<100 时 (max,100] 不得绕过钳制（A9 原缺陷）"},
		{150, 20, 200, 150, "max>100 时区间内不得被放大到 max（A9 原缺陷反向）"},
		{100000, 20, 100, 100, "超限钳到 max"},
	}
	for _, c := range cases {
		if got := clampPageSize(c.in, c.def, c.max); got != c.want {
			t.Fatalf("%s: clampPageSize(%d,%d,%d)=%d want %d", c.what, c.in, c.def, c.max, got, c.want)
		}
	}
}

func seedFinding(t *testing.T, repo *repository.MemoryFindingRepository, id, taskID string) {
	t.Helper()
	if err := repo.Create(&model.Finding{ID: id, TaskID: taskID, FilePath: "a.py", LineNumber: 1}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestMemoryList_NoPhantomNextOnExactTail(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	for i := 0; i < 5; i++ {
		seedFinding(t, repo, string(rune('a'+i)), "t-1")
	}
	// 尾页恰好剩 5 条、limit=5：PG 版 limit+1 探测判"无下一页"——内存档原实现恒置 next
	out, next, err := repo.List("", 5, "t-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("want 5 findings, got %d", len(out))
	}
	if next != "" {
		t.Fatalf("恰好填满尾页时产出幻影 next=%q——客户端会再拉一页空结果（P3-3）", next)
	}
	// limit=4 时应有 next 且续页不重不漏
	out, next, _ = repo.List("", 4, "t-1", "", "")
	if len(out) != 4 || next != "d" {
		t.Fatalf("limit=4: out=%d next=%q（want 4, d）", len(out), next)
	}
	out2, next2, _ := repo.List(next, 4, "t-1", "", "")
	if len(out2) != 1 || next2 != "" {
		t.Fatalf("续页: out=%d next=%q（want 1, 空）", len(out2), next2)
	}
}

func TestMemoryList_GhostCursorNoReplay(t *testing.T) {
	repo := repository.NewMemoryFindingRepository()
	seedFinding(t, repo, "a", "t-1")
	seedFinding(t, repo, "b", "t-1")
	// 游标引用不存在的条目（已删/伪造）：原实现 idx 归零从头重放=整页重复
	out, next, err := repo.List("ghost", 10, "t-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || next != "" {
		t.Fatalf("幽灵游标须诚实空页（原实现从头重放 %d 条=重复下发）", len(out))
	}
}

func TestCreateFinding_PersistsReasoning(t *testing.T) {
	s := NewResultServiceImpl(repository.NewMemoryFindingRepository())
	fid := "f-r138-reasoning"
	_, err := s.CreateFinding(context.Background(), &pb.CreateFindingRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-r138-reasoning"},
		Finding: &pb.UnifiedFinding{
			FindingId: fid, TaskId: "t-1", SourceTool: "bandit",
			Location: &pb.LocationInfo{FilePath: "a.py", StartLine: 1},
			Severity: pb.Severity_SEVERITY_HIGH,
			// R138/P3-1：单条通道此前静默丢弃该字段（Batch 分支有落）
			AiReasoning: "[LLM:] 该 sink 的用户输入未经过滤即进入 eval",
		},
	})
	if err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	got, err := s.GetFinding(context.Background(), &pb.GetFindingRequest{FindingId: fid})
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.GetFinding().GetAiReasoning() == "" {
		t.Fatalf("单条 CreateFinding 丢失 ai_reasoning（与 Batch 分支不同口径，P3-1）")
	}
}
