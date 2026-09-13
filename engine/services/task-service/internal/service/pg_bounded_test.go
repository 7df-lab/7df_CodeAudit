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
	if !strings.Contains(string(src), "ExecContext(ctx,") {
		t.Fatal("R77: upsert 未走带超时 ctx 的 ExecContext（锁内无界 PG 写回归）")
	}
	if strings.Contains(string(src), "st.db.Exec(`") {
		t.Fatal("R77: 裸 Exec 回潮（无超时上限）")
	}
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
