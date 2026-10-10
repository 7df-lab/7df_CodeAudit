package service

import (
	"os"
	"strings"
	"testing"
)

// R-30 锁定（先红后绿）：findings 表 DDL 有 reasoning 列（ADR-135 迁移也补了列），
// 但 PostgresFindingRepository 三条 SQL 路径全漏——Create 的 INSERT 不写、行投影
// SELECT 不读、Update 的 SET 不更。后果：
//
//	① AI 创建期结论原文（[DSH-sandbox]/[LLM:] 前缀，ADR-140/166）落不了库 →
//	  前端 isAIReasoning 恒 false → ADR-195 链路点选（风险详情"定位 sink 链"）在
//	  真数据上从未渲染；
//	② 人工裁决 UpdateVerdict 的 reasoning 静默丢弃（verdict 落库、理由丢）→
//	  P4"人工裁决理由必须原文展示"失效。
//
// memory 仓 Update 是整结构体拷贝，单测全绿测不出——只有部署形态（PG）暴露，
// 故用文本面契约锁定（同 R-29 Dockerfile 契约模式）。
//
// 2026-10-09 判定两轴重构升级：行投影 SELECT 列清单收敛为 findingColumns 常量
// （原 8 分支拷贝式 SQL → 动态 WHERE 单模板），契约随之升级为更强的
// **列清单单一事实源**断言：
//   - findingColumns 必须含 reasoning（COALESCE NULL 安全）与两轴
//     verdict_direction/verdict_tier；
//   - 所有行投影查询必须经 findingColumns 拼接——仓内不得残留内联
//     "SELECT id, task_id" 列清单（列清单分叉 = 某查询漏读新列的温床）。
//
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
			if !strings.Contains(c, "verdict_direction") || !strings.Contains(c, "verdict_tier") {
				t.Errorf("INSERT 语句缺判定两轴列——创建期判定将丢失（2026-10-09 重构）")
			}
		case strings.Contains(c, "UPDATE findings") && strings.Contains(c, "SET task_id"):
			// 迁移 DO 块中的回填 UPDATE（SET verdict_direction…）不属业务 Update 方法，排除
			updates++
			if !strings.Contains(c, "reasoning") {
				t.Errorf("UPDATE 语句缺 reasoning 列——人工裁决理由将被丢弃（R-30②）")
			}
			if !strings.Contains(c, "verdict_direction") || !strings.Contains(c, "verdict_tier") {
				t.Errorf("UPDATE 语句缺判定两轴列——人工裁决将丢失（2026-10-09 重构）")
			}
		case strings.Contains(c, "SELECT") && strings.Contains(c, "FROM findings"):
			selects++
		}
	}
	// 行投影统一走 findingColumns 拼接（GetByID/GetByRequestID/List 三查询族）；
	// 聚合 COUNT 查询不含 findingColumns 也不含 id 行投影，单独计数
	if selects < 1 {
		t.Errorf("行投影 SELECT 数量 %d < 1——查询被删？", selects)
	}
	if inserts != 1 {
		t.Errorf("INSERT 语句数 %d != 1", inserts)
	}
	if updates != 1 {
		t.Errorf("UPDATE 语句数 %d != 1", updates)
	}
	// 列清单单一事实源：findingColumns 常量必须齐备全部行投影列
	mustHave := []string{
		"id, task_id",
		"verdict_direction, verdict_tier",
		"COALESCE(reasoning, '')",
		"COALESCE(inherited_from_task_id, '')",
	}
	for _, m := range mustHave {
		if !strings.Contains(src, "const findingColumns") || !strings.Contains(src[strings.Index(src, "const findingColumns"):], m) {
			t.Errorf("findingColumns 常量缺列清单片段 %q——行投影列清单单一事实源被破坏", m)
		}
	}
	// 仓内不得残留内联列清单（"SELECT id, task_id" 直写 = 绕过 findingColumns 分叉）
	if strings.Contains(src, "SELECT id, task_id") {
		t.Errorf("残留内联行投影列清单（SELECT id, task_id…）——必须经 findingColumns 拼接（2026-10-09 收敛）")
	}
	// 列进了语句，参数侧（Scan/Exec）也必须接线：scanFinding 单点 Scan
	if !strings.Contains(src, "func scanFinding") {
		t.Errorf("scanFinding 单点 Scan 缺失——行投影参数侧接线分叉")
	}
	scanBody := src[strings.Index(src, "func scanFinding"):]
	for _, frag := range []string{"&f.Reasoning", "&f.VerdictDirection", "&f.VerdictTier", "&f.InheritedFrom"} {
		if !strings.Contains(scanBody, frag) {
			t.Errorf("scanFinding Scan 缺 %s——读回恒零值", frag)
		}
	}
	if strings.Count(src, "finding.Reasoning") < 2 {
		t.Errorf("Exec 参数侧缺 finding.Reasoning（Create INSERT 参数 + Update SET 参数，共应 ≥2 处；Scan 侧已收敛 scanFinding 单点）")
	}
}
