package service

// R115：taskByIDRW 读口换入守卫。仅 PG
// 严格新于内存时才换入——同副本旧读不得覆盖本地写者的活实体（面孔①降级键丢/
// 面孔②stage type 回水 UNSPECIFIED 的共同根因）；跨副本传播由"严格新"分支保留
// （DOOR3 目标不变）。时间戳权威=PG updated_at 列（now()，跨副本同钟）：
// getTask/hydrateTasks 盖 proto.UpdatedAt，upsert RETURNING 回填写穿后的内存实体。

import (
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/codeaudit/proto-gen"
)

func tsAgo(minAgo int) *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(-time.Duration(minAgo) * time.Minute))
}

func TestChooseLiveEntity_R115(t *testing.T) {
	mem := &pb.ScanTask{TaskId: "t-1", UpdatedAt: tsAgo(5),
		Config: map[string]string{"incremental_degraded_reason": "no_baseline"}}
	mem.Stages = []*pb.TaskStage{{StageId: "sast", Type: pb.StageType_STAGE_TYPE_SAST_SCAN}}
	pgStale := &pb.ScanTask{TaskId: "t-1", UpdatedAt: tsAgo(6)} // 旧读：无键无类型
	pgNewer := &pb.ScanTask{TaskId: "t-1", UpdatedAt: tsAgo(1),
		Status: pb.TaskStatus_TASK_STATUS_RUNNING}

	t.Run("PG旧读不得覆盖活实体（面孔①②根因）", func(t *testing.T) {
		if got := chooseLiveEntity(mem, pgStale); got != mem {
			t.Fatalf("陈旧 PG 读换入覆盖活实体——R115 回归")
		}
	})
	t.Run("同刻读不得换入（竞态窗口）", func(t *testing.T) {
		eq := &pb.ScanTask{TaskId: "t-1", UpdatedAt: mem.UpdatedAt}
		if got := chooseLiveEntity(mem, eq); got != mem {
			t.Fatalf("同刻 PG 读换入——与本地未落盘变更的竞争窗口复活")
		}
	})
	t.Run("PG严格新→换入（DOOR3 跨副本传播保留）", func(t *testing.T) {
		if got := chooseLiveEntity(mem, pgNewer); got != pgNewer {
			t.Fatalf("跨副本新状态未传播——DOOR3 目标回退")
		}
	})
	t.Run("零值组合保守取内存", func(t *testing.T) {
		if got := chooseLiveEntity(mem, &pb.ScanTask{TaskId: "t-1"}); got != mem {
			t.Fatal("PG 无版本时间戳时不得换入")
		}
		noTs := &pb.ScanTask{TaskId: "t-1"}
		if got := chooseLiveEntity(noTs, pgNewer); got != pgNewer {
			t.Fatal("内存无版本而 PG 有→取 PG（防御面）")
		}
		if got := chooseLiveEntity(noTs, &pb.ScanTask{TaskId: "t-1"}); got != noTs {
			t.Fatal("双零→保守保内存")
		}
	})
	t.Run("nil面", func(t *testing.T) {
		if got := chooseLiveEntity(nil, pgNewer); got != pgNewer {
			t.Fatal("mem nil → 应取 pg")
		}
		if got := chooseLiveEntity(mem, nil); got != mem {
			t.Fatal("pg nil → 应保 mem")
		}
	})
}

// 接线契约（pg_bounded_test.go funcSeg 静态段同款手法）：taskByIDRW 的 PG 命中
// 分支必须经 chooseLiveEntity 守卫换入，不得无条件直写映射表（内存 miss 分支的
// 双查灌入不在本守卫面）。
func TestTaskByIDRW_WiringGuarded(t *testing.T) {
	src, err := os.ReadFile("task_service.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := funcSeg(t, string(src), "func (s *TaskServiceImpl) taskByIDRW")
	i := strings.Index(fn, "if ok {")
	j := strings.Index(fn, "// PG 抖动")
	if i < 0 || j < 0 || j < i {
		t.Fatalf("taskByIDRW PG 命中分支段定位失败（i=%d j=%d）", i, j)
	}
	hitBranch := fn[i:j]
	if !strings.Contains(hitBranch, "chooseLiveEntity(") {
		t.Fatal("R115: taskByIDRW PG 命中分支未经理守卫换入（chooseLiveEntity 缺席）")
	}
	if strings.Contains(hitBranch, "s.tasks[taskID] = t") {
		t.Fatal("R115: PG 命中分支仍存在无条件换入直写（s.tasks[taskID] = t）")
	}
}
