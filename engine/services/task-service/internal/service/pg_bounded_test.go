package service

// R77/R83 锁定测试（结构性守卫：行为面在 verify G2 全链路覆盖，此处锁接线不变量）。
// R77: upsert 在全局写锁内执行——必须走带超时 ctx 的 ExecContext（裸 Exec 在 PG 抖动时
// 冻结创建/列表/快照/流式全任务面，R47 同型）。
// R83（R70② 补实）: main 必须接线 producer.Close()（停机冲刷在途事件批）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPGUpsert_BoundedInLock(t *testing.T) {
	src, err := os.ReadFile("task_store_pg.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// 2026-09-19 修正（ADR-247 批）：原全文件匹配在 task_store_pg.go 新增
	// replaceUsage/queryUsageRows 后失效——变异摘除 upsert 超时 ctx，同文件新函数的
	// ExecContext(ctx, 仍喂绿守卫（M51 存活实证）。改函数段匹配，并顺带把
	// replaceUsage（ADR-247 usage 归集）纳入同源超时纪律。
	upsertSeg := funcSeg(t, body, "func (st *pgTaskStore) upsert")
	// R115 修订：upsert 改 QueryRowContext（RETURNING updated_at
	// 供换入守卫做版本基准）——R77 不变量=锁内调用必须 ctx 有界，非绑定具体方法；
	// 守卫随之接受 ExecContext/QueryRowContext 任一 ctx 绑定形态。
	if !(strings.Contains(upsertSeg, "ExecContext(ctx,") || strings.Contains(upsertSeg, "QueryRowContext(ctx,")) || !strings.Contains(upsertSeg, "context.WithTimeout") {
		t.Fatal("R77: upsert 未走带超时 ctx 的锁内 PG 调用（无界写回归）")
	}
	replaceSeg := funcSeg(t, body, "func (st *pgTaskStore) replaceUsage")
	if !strings.Contains(replaceSeg, "ExecContext(ctx,") || !strings.Contains(replaceSeg, "context.WithTimeout") {
		t.Fatal("ADR-247: replaceUsage 事务写未走带超时 ctx（同源纪律）")
	}
	if strings.Contains(body, "st.db.Exec(`") {
		t.Fatal("R77: 裸 Exec 回潮（无超时上限）")
	}
}

// funcSeg — 提取函数体段（从签名锚到首个顶格 "}"）。
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

func TestMainWiresProducerClose(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "defer producer.Close()") {
		t.Fatal("R83/R70②: main 未接线 producer.Close()（停机丢在途事件批，台账漂移回潮）")
	}
}
