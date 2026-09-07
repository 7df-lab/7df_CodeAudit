package service

import (
	"os"
	"strings"
	"testing"
)

// R-30 锁定（先红后绿）：findings 表 DDL 有 reasoning 列（ADR-135 迁移也补了列），
// 但 PostgresFindingRepository 三条 SQL 路径全漏——Create 的 INSERT 不写、行投影
// SELECT 不读、Update 的 SET 不更。后果（2026-09-07 sim 实证）：
//   ① AI 创建期结论原文（[DSH-sandbox]/[LLM:] 前缀，ADR-140/166）落不了库 →
//     前端 isAIReasoning 恒 false → ADR-195 链路点选（风险详情"定位 sink 链"）在
//     真数据上从未渲染；
//   ② 人工裁决 UpdateVerdict 的 reasoning 静默丢弃（verdict 落库、理由丢）→
//     P4"人工裁决理由必须原文展示"失效。
// memory 仓 Update 是整结构体拷贝，单测全绿测不出——只有部署形态（PG）暴露，
// 故用文本面契约锁定（同 R-29 Dockerfile 契约模式）。
//
// 契约：INSERT INTO findings / UPDATE findings / 行投影 SELECT（SELECT id, … FROM
// findings）三类语句必须携带 reasoning 列，且 Scan/Exec 参数侧同步接线。读路径用
// COALESCE(reasoning,'')——存量行该列为 NULL，裸读 Scan 进 string 直接崩。
// 聚合查询（GetStatsByTaskID，SELECT COUNT…）不属行投影，豁免。
func TestFindingRepoReasoningWired(t *testing.T) {
	b, err := os.ReadFile("../repository/finding_repository.go")
	if err != nil {
		t.Fatalf("读 finding_repository.go: %v", err)
	}
	src := string(b)

	// Go raw string（反引号）交替切分：奇数下标 = SQL 块
	chunks := strings.Split(src, "`")
	var selects, inserts, updates int
	for i := 1; i < len(chunks); i += 2 {
		c := chunks[i]
		switch {
		case strings.Contains(c, "INSERT INTO findings"):
			inserts++
			if !strings.Contains(c, "reasoning") {
				t.Errorf("INSERT 语句缺 reasoning 列——创建期 AI 结论原文将丢失（R-30①）")
			}
		case strings.Contains(c, "UPDATE findings"):
			updates++
			if !strings.Contains(c, "reasoning") {
				t.Errorf("UPDATE 语句缺 reasoning 列——人工裁决理由将被丢弃（R-30②）")
			}
		case strings.Contains(c, "SELECT id, task_id") && strings.Contains(c, "FROM findings"):
			selects++
			if !strings.Contains(c, "reasoning") {
				t.Errorf("行投影 SELECT 缺 reasoning——读回恒空（ADR-195 链路点选/人工理由展示失效）")
			}
			if !strings.Contains(c, "COALESCE(reasoning, '')") {
				t.Errorf("行投影 SELECT 的 reasoning 未包 COALESCE——存量 NULL 行 Scan 进 string 即崩")
			}
		}
	}
	// 查询变体数下限：List 8 形态 + GetByID + GetByRequestIDAndFindingID = 10
	if selects < 10 {
		t.Errorf("行投影 SELECT 数量 %d < 10——查询变体被删（R-30 锁定面收窄？）", selects)
	}
	if inserts != 1 {
		t.Errorf("INSERT 语句数 %d != 1", inserts)
	}
	if updates != 1 {
		t.Errorf("UPDATE 语句数 %d != 1", updates)
	}
	// 列进了语句，参数侧（Scan/Exec）也必须接线
	if strings.Count(src, "&finding.Reasoning") < 2 {
		t.Errorf("GetByID/GetByRequestIDAndFindingID 的 Scan 缺 &finding.Reasoning")
	}
	if !strings.Contains(src, "&f.Reasoning") {
		t.Errorf("List 的 Scan 缺 &f.Reasoning")
	}
	if strings.Count(src, "finding.Reasoning") < 4 {
		t.Errorf("Exec 参数侧缺 finding.Reasoning（Create INSERT 参数 + Update SET 参数，共应 ≥4 处含 Scan）")
	}
}
