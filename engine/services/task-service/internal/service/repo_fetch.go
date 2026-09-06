// 仓库拉取模式（ADR-163；14号 Q4"仓库拉取"演进落地，人类裁决：仓库/上传双通道均须
// 支持四类扫描模式）。project_path 缺省且项目配置 repo_url 时，StartTask 编排协程
// 内前置 git clone --depth 1（不阻塞 RPC；失败走既有 FAILED→QUEUED 重试→DEAD 链）。
// 凭据边界：V1 依赖运行环境既有的 git 凭据（https/ssh/file），不做凭据管理。
package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// cloneRepo — git clone --depth 1 --single-branch 到 dest；失败清理半成品目录并携带
// git 输出片段报错（诚实失败）。返回 dest 供编排作为 project_path。
func cloneRepo(ctx context.Context, repoURL, branch, dest string, timeout time.Duration) (string, error) {
	if repoURL == "" {
		return "", fmt.Errorf("repo_url is empty")
	}
	if dest == "" {
		return "", fmt.Errorf("clone dest is empty")
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
	args = append(args, repoURL, dest)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "git", args...).CombinedOutput()
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
