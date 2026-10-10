package service

// 第四轮审计 P1-3 锁定测试：auto-retry 重试成功后阶段看板不得冻结在第一次失败
// 的 FAILED/SKIPPED（ADR-149b 当初修掉的症状在 auto-retry 路径回潮）——修复=
// 重试再入 Execute 前 registerStages+resetStages（与 StartTask 同口径）。

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
)

// retrySucceedOrch — 第 1 次执行把 analyze 阶段点成 RUNNING 后失败（复刻真实编排：
// 阶段一旦启动，失败收尾的 finalize 盖 FAILED——成功 finalize 对终态早退=冻结面）、
// 第 2 次成功（模拟下游瞬断后恢复）。不驱动 recorder 的纯失败首试只会留 SKIPPED，
// 而成功 finalize 会把 SKIPPED 重盖 COMPLETED（自愈）——测不出冻结（M123 存活实录）。
type retrySucceedOrch struct{ calls int }

func (f *retrySucceedOrch) Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error) {
	f.calls++
	if f.calls == 1 {
		if r.Recorder != nil {
			r.Recorder("analyze", "attempt 1 in flight")
		}
		return nil, context.DeadlineExceeded // 任意非 nil 错误即触发 auto-retry 链
	}
	return map[string]interface{}{}, nil
}

func TestAutoRetrySuccess_StageBoardNotFrozenInFailure(t *testing.T) {
	s := newSvc(t)
	fake := &retrySucceedOrch{}
	s.orch = fake // R75 seam

	if _, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-retry-ok"},
		ProjectId: "p-retry-ok",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"project_path": "/tmp/rt"},
	}); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s, "req-retry-ok")
	res := waitForStatus(t, s, "req-retry-ok", pb.TaskStatus_TASK_STATUS_COMPLETED, 30*time.Second)
	if fake.calls != 2 || int(res.GetRetryCount()) != 1 {
		t.Fatalf("应执行 2 次（首次失败+1 重试成功）: calls=%d retry=%d", fake.calls, res.GetRetryCount())
	}
	// 核心断言：终态任务=COMPLETED，阶段板不得残留 FAILED/SKIPPED（第一次尝试的盖章）
	for _, st := range res.GetStages() {
		if st.GetStatus() == pb.StageStatus_STAGE_STATUS_FAILED || st.GetStatus() == pb.StageStatus_STAGE_STATUS_SKIPPED {
			t.Fatalf("任务 COMPLETED 但阶段 %s 冻结在 %s（auto-retry 未归零阶段看板，ADR-149b 回潮）",
				st.GetStageId(), st.GetStatus())
		}
	}
}
