// sandbox_incremental.go — 增量聚焦提示词（ADR-225 D3：AI 全量上下文 + 增量聚焦）。
//
// 依据: 伞仓 docs/designs/incremental-scan.md §4.7——注入点=sandbox.Task.Assignment
//（生产先例 sandboxAssignmentReview/fixretry 的清单 JSON 拼接同款通道）；
// 沙箱代码树仍为全量（tar 链路不变），任务卡注入变更清单+diff，令 AI 集中审查
// 变更代码的安全风险，同时保留跨文件追踪指路。注入内容随第一条 /prompt 全文
// 进入 .ai.log「📋 [任务下发]」帧（生效可审计）。
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

// incrementalPromptBudget — 任务卡内联 diff 预算（ADR-187"代码全文不进 prompt"的
// 同源纪律）：超预算全文落项目树文件，任务卡只留截断版+指路。
const incrementalPromptBudget = 24 << 10

// incrementalDiffFileName — 大 diff 落项目树的文件名（与 task-service 内容 diff 的
// 排除名单同名互镜，防其混入下一轮基线对比；tarProject/walkExcludes 不拦该文件名，
// 随包进沙箱后 AI 可直接读取）。
const incrementalDiffFileName = ".codeaudit-incremental.diff"

// isIncrementalAssignment — 生产判据（ai_engine 消费）：任一增量字段非空即注入
// 增量任务卡；全空 → 与现状 ModeA 逐字节一致（A8.3 非增量回归口径）。
func isIncrementalAssignment(changed, deleted []string, incDiff string) bool {
	return len(changed) > 0 || len(deleted) > 0 || incDiff != ""
}

// sandboxAssignmentIncremental — 模式A 主审计任务卡 + 增量段。
// projectPath 为宿主侧项目根（写 diff 文件用）；沙箱内路径固定 /sandbox/project。
func sandboxAssignmentIncremental(changed, deleted []string, incDiff, projectPath string) string {
	var b strings.Builder
	b.WriteString(sandboxAssignmentModeA())
	b.WriteString("\n## 增量扫描重点（相对上次扫描的代码变更）\n")
	b.WriteString(fmt.Sprintf(
		"本任务为增量扫描。请将审查重点放在下列变更代码的安全风险上（注入/鉴权缺失/敏感信息/不安全反序列化等）；"+
			"完整代码库位于 %s 目录，追踪跨文件数据流与调用关系时仍须使用全量上下文（变更可能引入跨文件污点链路）。\n",
		sandbox.ProjectSandboxPath))
	if len(changed) == 0 && len(deleted) == 0 {
		b.WriteString("本次零代码变更：无需新扫描，如实返回空结论。\n")
		return b.String()
	}
	if len(changed) > 0 {
		fmt.Fprintf(&b, "变更文件（新增/修改，共 %d 个）：\n", len(changed))
		for _, p := range changed {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(deleted) > 0 {
		fmt.Fprintf(&b, "删除文件（共 %d 个，安全影响=被删防护/校验逻辑）：\n", len(deleted))
		for _, p := range deleted {
			b.WriteString("- " + p + "\n")
		}
	}
	inline := incDiff
	if inline != "" && len(inline) > incrementalPromptBudget {
		fileRef := ""
		if werr := os.WriteFile(filepath.Join(projectPath, incrementalDiffFileName), []byte(inline), 0o644); werr == nil {
			// walkExcludes 不拦该文件名 → 随 tar 进沙箱，AI 可整文件读取
			fileRef = fmt.Sprintf("完整 diff 已置于项目树 %s，可直接读取。", filepath.Join(sandbox.ProjectSandboxPath, incrementalDiffFileName))
		} else {
			fileRef = "完整 diff 写入项目树失败，以上文清单为准。"
		}
		inline = truncateAtLine(inline, incrementalPromptBudget) + "\n…（diff 超出任务卡预算已截断；" + fileRef + "）\n"
	}
	if inline != "" {
		b.WriteString("变更 diff（unified 格式，基线版本 → 本次版本）：\n" + inline + "\n")
	}
	return b.String()
}

// truncateAtLine — 按行边界截断（不撕裂 diff 行，AI 可读性优先）。
func truncateAtLine(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if idx := strings.LastIndex(cut, "\n"); idx > 0 {
		cut = cut[:idx]
	}
	return cut
}
