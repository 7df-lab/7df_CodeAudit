package orchestrator

// R116 P2 静态段守卫：runGraphTask 挂起分支必须返回 ErrGraphPaused 哨兵
// （return nil 会被终态迁移标 COMPLETED——R119 缺陷本体；fake orch 测试打不到
// 真 gRPC 分支，本守卫是 M88 的牙）。

import (
	"os"
	"strings"
	"testing"
)

func funcSegLocal(t *testing.T, body, anchor string) string {
	t.Helper()
	i := strings.Index(body, anchor)
	if i < 0 {
		t.Fatalf("函数段未找到: %s", anchor)
	}
	j := strings.Index(body[i:], "\n}")
	if j < 0 {
		t.Fatalf("函数段无界: %s", anchor)
	}
	return body[i : i+j]
}

func TestRunGraphTask_PausedBranchReturnsSentinel(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	whole := string(src)
	i := strings.Index(whole, "if resp.GetPaused()")
	j := strings.Index(whole, "if !resp.GetConverged()")
	if i < 0 || j < 0 || j < i {
		t.Fatalf("挂起分支段定位失败（i=%d j=%d）", i, j)
	}
	seg := whole[i:j]
	if !strings.Contains(seg, "return ErrGraphPaused") {
		t.Fatal("R116: 挂起分支未返回 ErrGraphPaused 哨兵（COMPLETED 假完成回归）")
	}
	if strings.Contains(seg, "return nil") {
		t.Fatal("R116: 挂起分支残留 return nil（nil=成功语义）")
	}
}
