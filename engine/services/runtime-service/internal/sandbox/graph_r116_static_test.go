package sandbox

// R116 静态段守卫（pg_bounded_test.go funcSeg 手法）+ nodeSeqOf 放宽锁。
// 依据: 主仓库 .agent/evidence/baton-review-20260920/report.md §五 P3 顺手批。

import (
	"os"
	"strings"
	"testing"
)

// funcSeg — 函数体段提取（签名锚到首个顶格 "}"；task-service pg_bounded_test 同款）。
func funcSeg(t *testing.T, body, anchor string) string {
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

// nodeSeqOf 原钉死 len==4（n001..n999）——四位编号（n0007 形态，max_nodes>999
// 触发）回放编号接续断裂。放宽后 n+任意位数字。
func TestNodeSeqOf_WideIDs(t *testing.T) {
	cases := map[string]int{"n001": 1, "n0007": 7, "n12345": 12345, "x01": 0, "n": 0, "nabc": 0}
	for id, want := range cases {
		if got := nodeSeqOf(id); got != want {
			t.Fatalf("nodeSeqOf(%q)=%d, want %d", id, got, want)
		}
	}
}

// emit 全程持锁（R116：原版锁外写盘，并发发射 seq 乱序落盘=潜伏竞态）；
// appendGraphFile 全量写校验+Sync（graph.log 是 resume 唯一输入，撕裂成本高于
// .sse.log；R116 读侧容忍+写侧防撕裂双面）。
func TestGraphWritePath_StaticGuards(t *testing.T) {
	src, err := os.ReadFile("graph_events.go")
	if err != nil {
		t.Fatal(err)
	}
	emitSeg := funcSeg(t, string(src), "func (e *graphEmitter) emit")
	if !strings.Contains(emitSeg, "defer e.mu.Unlock()") {
		t.Fatal("R116: emit 未全程持锁（锁外写盘竞态回潮）")
	}
	src2, err := os.ReadFile("../service/ai_interaction_log.go")
	if err != nil {
		t.Fatal(err)
	}
	gSeg := funcSeg(t, string(src2), "func (e *aiLogEntry) appendGraphFile")
	if !strings.Contains(gSeg, "f.Sync()") || !strings.Contains(gSeg, "io.ErrShortWrite") {
		t.Fatal("R116: appendGraphFile 缺全量校验/Sync（撕裂面回潮）")
	}
	wSeg := funcSeg(t, string(src2), "func (e *aiLogEntry) writeGraph")
	if !strings.Contains(wSeg, "appendGraphFile(e.graphPath") {
		t.Fatal("R116: writeGraph 未走 appendGraphFile（守卫写路径回潮）")
	}
	// 子串自匹配免疫：剥掉 appendGraphFile 后不得再出现裸 appendFile 调用
	if strings.Contains(strings.ReplaceAll(wSeg, "appendGraphFile", ""), "appendFile(") {
		t.Fatal("R116: writeGraph 残留裸 appendFile 调用")
	}
}
