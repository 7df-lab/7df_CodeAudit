package main

// D6单测：对账活跃度探针第三源——graph.log mtime 纳入。
// 图任务 executor 排队等待期主面日志静默（gw-5ec1376 误杀形态），前序节点事件
// 持续写 graph.log 必须被探针视为任务活跃。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLatestInteractionLogMtime_GraphLogThirdSource(t *testing.T) {
	dir := t.TempDir()
	// 场景一（gw-5ec1376 形态）：主面两文件陈旧（40m 前），graph.log 新鲜（1m 前）
	old := time.Now().Add(-40 * time.Minute)
	fresh := time.Now().Add(-1 * time.Minute)
	for _, name := range []string{"gw-x.ai.log", "gw-x.sse.log"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	gp := filepath.Join(dir, "gw-x.graph.log")
	if err := os.WriteFile(gp, []byte(`{"seq":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(gp, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	ts, ok := latestInteractionLogMtime(dir, "gw-x")
	if !ok {
		t.Fatal("graph.log 存在时探针必须命中（D6 回归=排队期图任务被 30m 误杀）")
	}
	if ts.Sub(old) < 30*time.Minute {
		t.Fatalf("应取 graph.log 新鲜时间戳: got %v (want ≈%v)", ts, fresh)
	}

	// 场景二：仅 graph.log 存在（图任务早期，主面未写）——同样命中
	dir2 := t.TempDir()
	gp2 := filepath.Join(dir2, "gw-y.graph.log")
	if err := os.WriteFile(gp2, []byte(`{"seq":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(gp2, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if _, ok := latestInteractionLogMtime(dir2, "gw-y"); !ok {
		t.Fatal("仅 graph.log 在场也应命中（图任务首节点事件即活跃信号）")
	}

	// 场景三：三文件全缺——ok=false（reconciler 回落 updated_at，语义不变）
	if _, ok := latestInteractionLogMtime(t.TempDir(), "gw-none"); ok {
		t.Fatal("无任何日志文件应 ok=false")
	}
}
