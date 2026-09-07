package fusion

// ADR-212 回归（内部包：需注入未导出 stages）：失败阶段（尤其 runStage 的
// panic 恢复路径）返回 nil ctx 时，07 §10 降级回退必须基于非空上下文——
// 此前 buildFallbackResult(nil) 二次 panic（ADR-133 的 panic→error 修复自身崩进程）。

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

type panickyStage struct{}

func (panickyStage) Name() string { return "boom" }
func (panickyStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	panic("stage boom")
}

func TestExecute_FirstStagePanic_FallbackNoPanic(t *testing.T) {
	p := &FusionPipeline{stages: []Stage{panickyStage{}}}
	sast := []*pb.UnifiedFinding{{FindingId: "f1"}, {FindingId: "f2"}}
	ai := []*pb.UnifiedFinding{{FindingId: "a1"}}
	res, err := p.Execute(context.Background(),
		&pb.FuseResultsRequest{TaskId: "t-panic"}, sast, ai)
	if err != nil {
		t.Fatalf("fallback path must swallow stage panic (07 §10): %v", err)
	}
	if res.GetTotalCount() != 3 {
		t.Fatalf("fallback must carry unfused inputs, got %d", res.GetTotalCount())
	}
}
