// ADR-163 回归：cloneRepo 真实 git clone（本地裸仓，离线可跑）——
// 正向：文件落地；坏 URL：诚实报错且清理半成品目录。
package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
}

func TestCloneRepo_LocalBare(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	src := filepath.Join(base, "src")
	bare := filepath.Join(base, "src.git")

	gitRun(t, "", "init", "-q", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", ".")
	gitRun(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init")
	gitRun(t, "", "clone", "-q", "--bare", src, bare)

	dest := filepath.Join(base, "dest")
	got, err := cloneRepo(ctx, bare, "main", dest, 30*time.Second)
	if err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, "app.py")); err != nil {
		t.Fatalf("cloned file missing: %v", err)
	}

	// 坏 URL：诚实报错，且不残留半成品目录
	badDest := filepath.Join(base, "bad")
	if _, err := cloneRepo(ctx, filepath.Join(base, "nope.git"), "", badDest, 5*time.Second); err == nil {
		t.Fatal("want error for nonexistent repo")
	}
	if _, serr := os.Stat(badDest); !os.IsNotExist(serr) {
		t.Fatalf("stale clone dir should be cleaned: %v", serr)
	}
}

// R74 锁定测试：clone 侧最后防线——scheme 白名单（ext:: 外置传输=容器内命令执行）
// + 前导 '-' 拒绝。断言锚定校验错误消息本体：git 自身的失败（如 transport not
// allowed）不含这些标记，防止"git 恰好也报错"让测试假绿。
func TestCloneRepo_RejectsUnsafeRepoTarget(t *testing.T) {
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := cloneRepo(ctx, " ext::sh -c exit 7", "", dest, time.Second); err == nil {
		t.Fatalf("前导空白 repo_url 未被拒绝")
	} else if !strings.Contains(err.Error(), "leading/trailing whitespace") {
		t.Fatalf("前导空白错误未锚定校验: %v", err)
	}
	if _, err := cloneRepo(ctx, "ext::sh -c exit 7", "", dest, time.Second); err == nil {
		t.Fatalf("ext:: 未被拒绝")
	} else if !strings.Contains(err.Error(), "not allowed (http/https/ssh/git/file)") {
		t.Fatalf("ext:: 错误未锚定白名单校验: %v", err)
	}
	if _, err := cloneRepo(ctx, "-oProxyCommand=evil", "", dest, time.Second); err == nil {
		t.Fatalf("前导 '-' repo_url 未被拒绝")
	} else if !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("前导 '-' 错误未锚定校验: %v", err)
	}
	if _, err := cloneRepo(ctx, "https://git.example/x.git", "-u exec", dest, time.Second); err == nil {
		t.Fatalf("前导 '-' branch 未被拒绝")
	} else if !strings.Contains(err.Error(), "branch must not start with '-'") {
		t.Fatalf("branch 错误未锚定校验: %v", err)
	}
}

// R74 回归面：file:// scheme 在白名单 + GIT_ALLOW_PROTOCOL + '--' 终止符下照常可克隆。
func TestCloneRepo_FileURLScheme(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "src.git")
	gitRun(t, "", "init", "-q", "--bare", bare)

	dest := filepath.Join(base, "dest")
	got, err := cloneRepo(context.Background(), "file://"+bare, "", dest, 30*time.Second)
	if err != nil {
		t.Fatalf("file:// 克隆失败: %v", err)
	}
	if got != dest {
		t.Fatalf("dest 回读不一致: %q", got)
	}
}
