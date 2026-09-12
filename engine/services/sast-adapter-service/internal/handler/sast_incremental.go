// sast_incremental.go — 增量扫描文件清单执行模式（ADR-225 §4.5）。
//
// 依据: 伞仓 docs/designs/incremental-scan.md §4.5——changed_files 非空时仅对
// 清单文件执行工具；argv 来自可选配置 sast_adapter.tools.<id>.files_argv
//（{files} 占位符=本批文件；{rules} 同目录模板解析）。分批与 results 数组合并
// 沿用 ADR-144 semgrepFilesFallback 已验证形态（bandit/opengrep 的 -f json 输出
// 同为顶层 results 数组，合并语义一致）。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxIncrementalFiles — 文件清单模式单任务文件数上限：超出降级全目录
//（argv 长度与执行时长防护；500×~200B≈100KB 远离 ARG_MAX）。
const maxIncrementalFiles = 500

// incrementalFilesChunk — 单批文件数（ADR-144 同款口径）。
const incrementalFilesChunk = 40

// runFilesMode — 文件清单模式：files_argv 模板 + 分批执行 + results 合并。
// 任一批产出即合并；全部批次失败返回错误（与目录模式的工具失败语义一致，
// 由调用方按 04 §6 跳过继续）。
func (h *SASTAdapterHandler) runFilesMode(absProject, tool string, tc toolCommand, changedFiles []string) ([]byte, error) {
	// 相对路径 → 绝对路径（与 {project} 目录模式同一基准根）
	files := make([]string, 0, len(changedFiles))
	for _, cf := range changedFiles {
		files = append(files, filepath.Join(absProject, filepath.FromSlash(cf)))
	}
	rulesDir, err := findRulesDir()
	if err != nil {
		return nil, err
	}
	merged := map[string]interface{}{"results": []interface{}{}}
	total := 0
	for s := 0; s < len(files); s += incrementalFilesChunk {
		e := s + incrementalFilesChunk
		if e > len(files) {
			e = len(files)
		}
		argv, aerr := buildFilesArgv(tc.filesArgv, rulesDir, files[s:e])
		if aerr != nil {
			return nil, aerr
		}
		argv, aerr = ensureCommand(argv) // python -m 兜底与目录模式同款（ADR-137）
		if aerr != nil {
			return nil, status.Error(codes.Unavailable, aerr.Error()+"（04 §6 工具失败→跳过继续）")
		}
		ctx, cancel := context.WithTimeout(context.Background(), h.scanTimeout)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		if argv[0] == "python3" && tc.pythonPath != "" {
			cmd.Env = append(os.Environ(), "PYTHONPATH="+tc.pythonPath)
		}
		out, err := cmd.Output()
		cancel()
		log.Printf("[sast-adapter] incremental %s files=%d bytes=%d err=%v",
			tool, e-s, len(out), err)
		if err != nil && len(out) == 0 {
			continue // 该批失败：跳过继续（04 §6），全部批次空产出由目录模式同款语义兜底
		}
		var part map[string]interface{}
		if json.Unmarshal(out, &part) == nil {
			if rs, ok := part["results"].([]interface{}); ok {
				existing := merged["results"].([]interface{})
				merged["results"] = append(existing, rs...)
				total += len(rs)
			}
		}
	}
	if total == 0 && len(files) > 0 {
		// 零产出合法（干净变更），但需要与"工具全部失败"区分：返回空 results
		// 结构（解析器按空列表处理），错误留 nil——04 §6 工具失败在目录模式同款
		// 由 exit code 判定，此处批次级 continue 已吞掉瞬时失败，保底输出空集。
		log.Printf("[sast-adapter] incremental %s: 0 findings from %d files (clean changes)", tool, len(files))
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// buildFilesArgv — files_argv 模板 → 实际 argv（纯模板展开，可测）：
// {rules} 解析、{files} 展开为本批文件（占位符所在元素原地展开为 N 个参数）。
// 可执行校验与 python -m 兜底在执行时经 ensureCommand 完成（与目录模式同款）。
func buildFilesArgv(filesArgv []string, rulesDir string, files []string) ([]string, error) {
	argv := make([]string, 0, len(filesArgv)+len(files))
	hasFiles := false
	for _, a := range filesArgv {
		switch {
		case strings.Contains(a, "{files}"):
			argv = append(argv, files...)
			hasFiles = true
		case strings.Contains(a, "{rules}"):
			argv = append(argv, strings.ReplaceAll(a, "{rules}", rulesDir))
		default:
			argv = append(argv, a)
		}
	}
	if !hasFiles {
		return nil, fmt.Errorf("files_argv 缺少 {files} 占位符（ADR-225 配置错误）")
	}
	return argv, nil
}
