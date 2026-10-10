// fixretry — 补丁回合管线：
//
//	第二阶段 applyPatchPhase — 正式补丁生成：一阶段确认的 findings（diff_patch 恒空）
//	统一构建清单（index/title/file_path/start_line/fix_suggestion——不带 reasoning 全文，
//	补丁只需位置与意图）生成补丁；
//	第三阶段 retryFailedPatches — 失败反馈再生成（原 fixretry 全貌，ADR-183 补遗②）：
//	非空但校验失败的补丁走一轮 Cline 式自纠，触发条件不变、复验闸门不豁免。
//
// Cline 的机制（源码依据 cline/sdk/.../apply-patch.ts + apply-patch-parser.ts）：
// 校验失败的 hunk 生成具体 warning（哪个文件、哪个 hunk、最高相似度、≤200 字符上下文
// 预览）→ computePatchChanges 见 warning 即整体拒绝 → 错误字符串作为 tool error 回传
// 模型 → 模型看着失败原因在下一轮重新出补丁（retryable:false——不盲重试，重试是
// 模型基于具体反馈的自纠）。
//
// 平台映射：校验失败的补丁不再静默丢弃，而是把逐项失败详情（含相似度与上下文预览）
// 作为一次专项沙箱再生成回合的输入，让 DSH 自纠；返回补丁经同一严格闸门
// （NormalizeDiffPatch fuzz=0）复验后方可落盘——再生成不豁免校验。
// 恰好一轮（对齐 Cline 每次失败反馈换一轮自纠；失败两次如实丢弃，finding 保留）。
//
// 依据: ADR-183 补遗②+ ADR-241（两段式）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// patchRoundFn — 一轮沙箱回合的抽象（生产=ManagerRunner.Run 且 Task.PatchFixRound=true，
// Run 按 patches 语义解析返回结构化产出；测试=canned 产出注入）。
type patchRoundFn func(ctx context.Context, assignment string) ([]sandbox.PatchFix, error)

// patchFailure — 一项校验失败（findings 下标 + 失败详情）。
type patchFailure struct {
	index int
	err   error
}

// applyPatchPhase — 第二阶段（补丁生成，ADR-241）：一阶段 findings（diff_patch 恒空）
// 统一生成补丁。按 index 合并回 findings：
//   - NormalizeDiffPatch 通过 → 规范化补丁原位写入；
//   - 校验失败 → raw 原样保留（第三阶段 retryFailedPatches 的触发条件"非空且校验失败"
//     自然接住自纠——两段管线在此无缝衔接）；
//   - 模型显式空串 → 保持空（无把握≠写坏，不代写——与三阶段同口径）；
//   - 越界 index → 丢弃并计数（模型幻觉下标不落盘）。
//
// 回合失败 = 诚实降级：findings 原样返回（全部无补丁），不静默伪装成功。
// 清单为空时零开销直接返回（不发回合）。
func applyPatchPhase(ctx context.Context, projectPath string, findings []sandbox.Finding, emit TaskLogFunc, round patchRoundFn) []sandbox.Finding {
	if len(findings) == 0 {
		return findings
	}
	type item struct {
		Index         int    `json:"index"`
		Title         string `json:"title"`
		FilePath      string `json:"file_path"`
		StartLine     int    `json:"start_line"`
		FixSuggestion string `json:"fix_suggestion"`
	}
	items := make([]item, 0, len(findings))
	for i := range findings {
		items = append(items, item{
			Index: i, Title: findings[i].Title, FilePath: findings[i].FilePath,
			StartLine: int(findings[i].StartLine), FixSuggestion: findings[i].FixSuggestion,
		})
	}
	raw, _ := json.Marshal(items)
	fixes, err := round(ctx, sandbox.BuildPatchPhasePrompt(string(raw)))
	if err != nil {
		if emit != nil {
			emit("warn", fmt.Sprintf("补丁生成回合失败（发现保留、补丁为空，诚实降级）: %v", err))
		}
		return findings
	}
	applied, empty, bad, dropped := 0, 0, 0, 0
	for _, f := range fixes {
		if f.Index < 0 || f.Index >= len(findings) {
			dropped++
			continue
		}
		if strings.TrimSpace(f.DiffPatch) == "" {
			empty++
			continue
		}
		if normalized, verr := NormalizeDiffPatch(f.DiffPatch, projectPath); verr == nil {
			findings[f.Index].DiffPatch = normalized
			applied++
		} else {
			// raw 落位（非空+校验失败）→ 三阶段 fixretry 触发条件不变即可接管自纠；
			// 三阶段仍失败则由 mapSandboxFindings 最终置空丢弃（finding 保留）。
			findings[f.Index].DiffPatch = f.DiffPatch
			bad++
		}
	}
	if emit != nil {
		emit("info", fmt.Sprintf("补丁生成结果：有效补丁 %d/%d（显式空 %d，坏补丁 %d 留自纠，越界丢弃 %d）",
			applied, len(findings), empty, bad, dropped))
	}
	return findings
}

// retryFailedPatches — 审计回合产出的 findings 中，非空但校验失败的 diff_patch 走一轮
// 失败反馈再生成；成功复验的原位替换，仍失败/未返回的保持原样（后续映射丢弃，finding 保留）。
// raw diff_patch 为空的项不参与（模型主动不给补丁≠补丁写坏，不代写）。
func retryFailedPatches(ctx context.Context, taskID, projectPath string, findings []sandbox.Finding, emit TaskLogFunc, round patchRoundFn) []sandbox.Finding {
	var failures []patchFailure
	for i := range findings {
		raw := strings.TrimSpace(findings[i].DiffPatch)
		if raw == "" {
			continue
		}
		if _, err := NormalizeDiffPatch(raw, projectPath); err != nil {
			failures = append(failures, patchFailure{index: i, err: err})
		}
	}
	if len(failures) == 0 {
		return findings
	}
	msg := fmt.Sprintf("diff_patch 校验失败 %d/%d 项，发起失败反馈再生成（Cline 式自纠，单轮）", len(failures), len(findings))
	if emit != nil {
		emit("warn", msg)
	}
	fixes, err := runPatchFixRound(ctx, taskID, projectPath, findings, failures, round)
	if err != nil {
		if emit != nil {
			emit("warn", fmt.Sprintf("补丁再生成回合失败（保持丢弃，finding 保留）: %v", err))
		}
		return findings
	}
	fixed := 0
	for _, f := range fixes {
		if f.Index < 0 || f.Index >= len(findings) {
			continue
		}
		normalized, verr := NormalizeDiffPatch(f.DiffPatch, projectPath)
		if verr != nil {
			continue // 再生成仍不过闸：如实丢弃该补丁，不豁免校验
		}
		findings[f.Index].DiffPatch = normalized
		fixed++
	}
	if emit != nil {
		emit("info", fmt.Sprintf("补丁再生成结果：复验通过 %d/%d（仍失败项保持丢弃，finding 保留）", fixed, len(failures)))
	}
	return findings
}

// runPatchFixRound — 构建失败反馈任务指令并执行一轮再生成回合。
func runPatchFixRound(ctx context.Context, taskID, projectPath string, findings []sandbox.Finding, failures []patchFailure, round patchRoundFn) ([]sandbox.PatchFix, error) {
	_ = taskID
	type item struct {
		Index           int    `json:"index"`
		Title           string `json:"title"`
		File            string `json:"file_path"`
		StartLine       int    `json:"start_line"`
		BadPatch        string `json:"bad_diff_patch"`
		ValidationError string `json:"validation_error"`
	}
	items := make([]item, 0, len(failures))
	for _, f := range failures {
		sf := findings[f.index]
		items = append(items, item{
			Index: f.index, Title: sf.Title, File: sf.FilePath, StartLine: int(sf.StartLine),
			BadPatch: sf.DiffPatch, ValidationError: f.err.Error(),
		})
	}
	raw, _ := json.Marshal(items)
	assignment := `以下是你在上一轮安全审计中产出的 diff_patch 补丁，经服务端按工作区真实文件逐行校验后未通过
（校验错误随附：含最相似程度与期望上下文预览）。请逐项重新生成补丁：以沙箱内 ` + sandbox.ProjectSandboxPath + ` 目录中的
项目源码为唯一事实源，上下文/删除行必须与源码文件逐字一致（含缩进），不要凭记忆改写。

格式规范（apply_patch 语法）：
- *** Begin Patch / *** Update File: <相对路径> / *** End Patch 框架；
- 行首仅三种：空格=上下文（逐字复制原文件行）、-=删除（逐字复制原文件行）、+=新增；
- "@@ <原文件真实存在的一行内容>" 作锚定提示（取改动块上方最近一行）；
- 不使用行号；同一文件多个改动块从上到下；文件尾改动用 "*** End of File"；
- 所有文本 NFC 归一，引号用 ASCII 直引号；一个 finding 的完整修复放一个补丁。

失败清单（JSON，index 对应你上轮 findings 数组下标）：
` + string(raw) + `

输出契约（必须遵守）：完成后调用 submit_patches 工具提交结果（参数 schema：
patches 数组，每项 {index: 下标整数, diff_patch: "apply_patch 补丁文本"}）；
补丁较多时分批连续调用（每批最多 4 条，服务端按 index 合并）。
正文只写简短说明，不要在正文里另写 JSON。
仅当工具不可用时才降级：把结果作为一个 JSON 代码块输出为最后一条消息，schema:
{"patches": [{"index": 下标整数, "diff_patch": "apply_patch 补丁文本"}]}
只重新生成失败的项。`
	fixes, err := round(ctx, assignment)
	if err != nil {
		return nil, err
	}
	return fixes, nil
}
