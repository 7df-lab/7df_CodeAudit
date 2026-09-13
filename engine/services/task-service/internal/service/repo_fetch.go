// 仓库拉取模式（ADR-163；14号 Q4"仓库拉取"演进落地，人类裁决：仓库/上传双通道均须
// 支持四类扫描模式）。project_path 缺省且项目配置 repo_url 时，StartTask 编排协程
// 内前置 git clone --depth 1（不阻塞 RPC；失败走既有 FAILED→QUEUED 重试→DEAD 链）。
// 凭据边界：V1 依赖运行环境既有的 git 凭据（https/ssh/file），不做凭据管理。
package service

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fetchProjectRepo — 查项目 repo_url/default_branch（GetProject, proto L890/L1184）。
func (s *TaskServiceImpl) fetchProjectRepo(projectID string) (url string, branch string, err error) {
	conn, err := grpc.Dial(s.projectAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	client := pb.NewProjectServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), projectConfigTimeout)
	defer cancel()
	resp, err := client.GetProject(ctx, &pb.GetProjectRequest{ProjectId: projectID})
	if err != nil {
		return "", "", err
	}
	return resp.GetRepoUrl(), resp.GetDefaultBranch(), nil
}

// allowedCloneSchemes — R74: clone 侧 scheme 白名单（与 project-service 入口校验
// 同口径；独立成最后一道防线：存量项目可早于校验存在，gRPC 直写可绕过网关）。
var allowedCloneSchemes = map[string]bool{
	"http": true, "https": true, "ssh": true, "git": true, "file": true,
}

// validateRepoTarget — R74: git `ext::<command>` 外置传输会在本容器执行任意命令
// （认证后 RCE）；前导 '-' 会被 git 解析为选项。scp 语法（无 scheme）放行。
func validateRepoTarget(repoURL, branch string) error {
	if repoURL != "" {
		if repoURL != strings.TrimSpace(repoURL) {
			return fmt.Errorf("repo_url must not contain leading/trailing whitespace")
		}
		if strings.HasPrefix(repoURL, "-") {
			return fmt.Errorf("repo_url must not start with '-'")
		}
		if u, err := url.Parse(repoURL); err == nil && u.Scheme != "" && !allowedCloneSchemes[u.Scheme] {
			return fmt.Errorf("repo_url scheme %q not allowed (http/https/ssh/git/file)", u.Scheme)
		}
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch must not start with '-'")
	}
	return nil
}

// cloneRepo — git clone --depth 1 --single-branch 到 dest；失败清理半成品目录并携带
// git 输出片段报错（诚实失败）。返回 dest 供编排作为 project_path。
func cloneRepo(ctx context.Context, repoURL, branch, dest string, timeout time.Duration) (string, error) {
	if repoURL == "" {
		return "", fmt.Errorf("repo_url is empty")
	}
	if dest == "" {
		return "", fmt.Errorf("clone dest is empty")
	}
	if err := validateRepoTarget(repoURL, branch); err != nil { // R74: ext:: RCE / 选项注入拒绝
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil { // 清上次失败/重试残留
		return "", fmt.Errorf("clean stale clone dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("mkdir repos dir: %w", err)
	}
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, "--", repoURL, dest) // R74: 终止选项解析（URL/目录前导 '-' 双保险）
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	// R74: GIT_ALLOW_PROTOCOL 白名单（git 原生硬约束，封死 ext:: 及未来新传输协议）
	cmd.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=http:https:ssh:git:file")
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dest)
		snippet := string(out)
		if len(snippet) > 200 {
			snippet = snippet[len(snippet)-200:] // 尾部含 git 真实错误行
		}
		return "", fmt.Errorf("git clone %s: %v: %s", repoURL, err, snippet)
	}
	return dest, nil
}
