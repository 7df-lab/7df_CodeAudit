package handler

// R138 锁定测试两件（第四轮审计遗留清偿批）：
//  1. P2-2 取消传播：runTool 以 RPC ctx 为父——任务取消链（CancelScanTask→
//     orchCtx→gRPC ctx）不得在工具执行处断掉（原 Background 派生=取消后每个
//     工具仍完整跑到 scanTimeout，白耗 CPU/IO；R67 传播链死端）。
//  2. P2-3b findingStore FIFO 护栏：缓存只进不出=无界增长（OOM 向量），超限逐最旧。

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunTool_ParentCancelKillsTool(t *testing.T) {
	h := &SASTAdapterHandler{
		scanTimeout: 30 * time.Second,
		toolCommands: map[string]toolCommand{
			"sleeper": {argv: []string{"sleep", "30"}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := h.runTool(ctx, t.TempDir(), "sleeper", nil)
	if err == nil {
		t.Fatalf("取消后工具执行应报错")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("父 ctx 取消未杀掉工具（执行 %v>5s）——取消链在此断掉（R67 死端）", el)
	}
	if status.Code(err) != codes.Canceled {
		t.Fatalf("取消应以 codes.Canceled 语义上抛: %v", err)
	}
}

func TestFindingCachePut_EvictsBeyondCap(t *testing.T) {
	h := &SASTFusionHandler{}
	n := findingCacheCap + 10
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("f-%06d", i)
		h.findingCachePut(id, &pb.UnifiedFinding{FindingId: id})
	}
	count := 0
	h.findingStore.Range(func(_, _ any) bool { count++; return true })
	if count > findingCacheCap {
		t.Fatalf("findingStore 超上限未驱逐: %d > %d（无界增长 OOM 向量）", count, findingCacheCap)
	}
	if len(h.findingOrder) > findingCacheCap {
		t.Fatalf("FIFO 序表超上限: %d > %d", len(h.findingOrder), findingCacheCap)
	}
}
