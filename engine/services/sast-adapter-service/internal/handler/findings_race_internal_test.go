package handler

// ADR-212 回归（内部包：需触及未导出 store/mu）：findingsOf 读路径与
// scanOneTool 写路径此前一读一写均需锁——读侧无锁时并发即 Go runtime
// "concurrent map read and map write" fatal（不可 recover，进程死亡）。
// 本用例在 -race 下锁死该竞态；无 -race 时至少验证锁序无死锁。

import (
	"fmt"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestFindingsOf_ConcurrentWithStoreWrite(t *testing.T) {
	h := NewSASTAdapterHandler("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.mu.Lock()
			h.store[fmt.Sprintf("f-%d", i)] = &pb.UnifiedFinding{FindingId: fmt.Sprintf("f-%d", i)}
			h.mu.Unlock()
		}
	}()
	for i := 0; i < 500; i++ {
		_ = h.findingsOf([]string{"f-1", "f-2", "missing"})
	}
	<-done
}
