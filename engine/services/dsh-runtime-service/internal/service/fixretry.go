// fixretry — ADR-183 补遗②：补丁失败反馈再生成（Cline 对话式自纠的平台映射）。
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
// 依据: ADR-183 补遗②（人类指令"参考学习 Cline 的成熟代码逻辑解决问题"）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

// patchRoundFn — 一轮沙箱回合的抽象（生产=ManagerRunner.Run；测试=canned 响应注入）。
type patchRoundFn func(ctx context.Context, assignment string) (finalText string, err error)

// patchFailure — 一项校验失败（findings 下标 + 失败详情）。
type patchFailure struct {
	index int
	err   error
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
			Index: f.index, Title: sf.Title, File: sf.FilePath, StartLine: sf.StartLine,
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
patches 数组，每项 {index: 下标整数, diff_patch: "apply_patch 补丁文本"}）。
正文只写简短说明，不要在正文里另写 JSON。
仅当工具不可用时才降级：把结果作为一个 JSON 代码块输出为最后一条消息，schema:
{"patches": [{"index": 下标整数, "diff_patch": "apply_patch 补丁文本"}]}
只重新生成失败的项。`
	finalText, err := round(ctx, assignment)
	if err != nil {
		return nil, err
	}
	fixes, perr := sandbox.ParsePatches(finalText)
	if perr != nil {
		return nil, perr
	}
	return fixes, nil
}
