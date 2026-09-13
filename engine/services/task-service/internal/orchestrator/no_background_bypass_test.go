package orchestrator

// R78 锁定测试（结构性守卫）：ADR-191 撤步骤超时后，编排 ctx 是所有下游 RPC 的唯一
// 中断机制——编排器内出现 context.Background() 即为取消旁路回潮
// （原 compensateFindings:192 / resultStats:847 两处：result 挂起即协程永久悬挂）。

import (
	"bytes"
	"os"
	"testing"
)

func TestOrchestrator_NoBackgroundCtxBypass(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"context.Background()", "context.TODO()"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Fatalf("R78: 编排器内出现 %s（取消旁路回潮——下游挂起即永久悬挂）", bad)
		}
	}
}
