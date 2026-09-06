// ADR-163 回归：cloneRepo 真实 git clone（本地裸仓，离线可跑）——
// 正向：文件落地；坏 URL：诚实报错且清理半成品目录。
package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
