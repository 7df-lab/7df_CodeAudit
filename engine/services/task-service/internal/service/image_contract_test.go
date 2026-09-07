package service

import (
	"os"
	"strings"
	"testing"
)

// R-29 锁定（先红后绿）：仓库拉取模式（ADR-163）在部署形态下恒 DEAD——repo_fetch.go
// 在 task 容器内 exec "git"，而运行时镜像层只装了 ca-certificates tzdata，任何
// repo_url 项目的任务都报 '"git" executable file not found in $PATH'（2026-09-07
// sim e2e 07 复现，上传流任务不受影响故长期隐形）。
//
// 契约（双向锚）：repo_fetch.go 仍在执行 git ⇔ Dockerfile 运行时层必须安装 git。
// 任一侧演进（删仓库拉取模式/改运行时基础镜像）必须同步另一侧与本测试。
func TestTaskImageContainsGit(t *testing.T) {
	src, err := os.ReadFile("repo_fetch.go")
	if err != nil {
		t.Fatalf("读 repo_fetch.go: %v", err)
	}
	// 测试 cwd = services/task-service/internal/service，Dockerfile 在服务根（两级上）
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("读 services/task-service/Dockerfile: %v", err)
	}

	// 侧一：编排层当前确实在容器内执行 git（exec.CommandContext(ctx, "git", …)）
	if !strings.Contains(string(src), `"git"`) {
		t.Fatalf("repo_fetch.go 不再引用 \"git\"——仓库拉取模式已演进，请同步更新本契约与 REGRESSIONS R-29")
	}

	// 侧二：运行时镜像层的 apk 安装行必须含 git（builder 层无 apk 行，全文件唯一）
	var apkLines []string
	for _, line := range strings.Split(string(df), "\n") {
		if strings.Contains(line, "apk --no-cache add") {
			apkLines = append(apkLines, line)
		}
	}
	if len(apkLines) == 0 {
		t.Fatalf("Dockerfile 缺 apk 安装行（运行时基础镜像已换？同步本契约）")
	}
	for _, line := range apkLines {
		for _, tok := range strings.Fields(line) {
			if tok == "git" {
				return // 契约满足
			}
		}
	}
	t.Fatalf("task 运行时镜像未安装 git——仓库拉取模式（repo_fetch.go exec git）部署形态下恒 DEAD（R-29）")
}
