package handler

// ADR-225 S5 ⑤流锁定测试（验收 F14）：tar 解包防护 / 缓存命中口径 / LRU 数学。

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkTarGz(t *testing.T, dest string, files map[string]string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tw.Close()
}

func TestUnpackTreeTarGz_Roundtrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tree.tar.gz")
	mkTarGz(t, src, map[string]string{"app.py": "a=1", "pkg/u.py": "b=2"})
	dest := filepath.Join(t.TempDir(), "unpacked")
	if err := unpackTreeTarGz(src, dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for p, want := range map[string]string{"app.py": "a=1", "pkg/u.py": "b=2"} {
		b, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil || string(b) != want {
			t.Fatalf("%s 回读失败: %v %q", p, err, string(b))
		}
	}
}

func TestUnpackTreeTarGz_TraversalContained(t *testing.T) {
	// ADR-145 safeJoin 同款语义：Clean("/"+name) 把 .. 吸收进根内——穿越条目
	// 不逃出根（落在根下），而非整包拒绝
	src := filepath.Join(t.TempDir(), "evil.tar.gz")
	mkTarGz(t, src, map[string]string{"../../escape.py": "x"})
	destRoot := t.TempDir()
	dest := filepath.Join(destRoot, "unpacked")
	if err := unpackTreeTarGz(src, dest); err != nil {
		t.Fatalf("穿越条目应被吸收（containment）而非报错: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "escape.py")); !os.IsNotExist(err) {
		t.Fatal("穿越文件逃出到解包目录之外（containment 失效）")
	}
	if _, err := os.Stat(filepath.Join(dest, "escape.py")); err != nil {
		t.Fatalf("吸收后应落在根内: %v", err)
	}
}

func TestSafeJoinTree_SymlinkRejected(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "link.py")
	if err := os.Symlink(filepath.Join(root, "real.py"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := safeJoinTree(root, "link.py"); err == nil {
		t.Fatal("软链条目必须被拒")
	}
}

func TestRehydrateFromTreeTar_CacheHit(t *testing.T) {
	// 缓存命中路径：不触网（storageConn=nil 也必须成功）+ root_via 如实标注
	old := ReposDir
	ReposDir = t.TempDir()
	defer func() { ReposDir = old }()
	unpacked := filepath.Join(rehydrateCacheRoot(), "t-1", "unpacked")
	mkTreeFiles(t, unpacked, map[string]string{"app.py": "a=1"})
	tr := &Transcoder{} // 零连接：命中缓存不需要后端
	root, via, err := tr.rehydrateFromTreeTar("t-1", "tar-any")
	if err != nil || via != "tree_tar_rehydrated" {
		t.Fatalf("缓存命中失败: via=%q err=%v", via, err)
	}
	if !strings.HasSuffix(filepath.ToSlash(root), "t-1/unpacked") {
		t.Fatalf("缓存根=%q", root)
	}
	// 无锚点 → 诚实错误（不伪装成功）
	if _, _, err := (&Transcoder{}).rehydrateFromTreeTar("t-2", ""); err == nil {
		t.Fatal("无锚点必须报错")
	}
	// 无 ReposDir → 诚实错误
	ReposDir = ""
	if _, _, err := (&Transcoder{}).rehydrateFromTreeTar("t-3", "tar-x"); err == nil {
		t.Fatal("无缓存位必须报错")
	}
}

func mkTreeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestEnforceRehydrateLRU_EvictsOldest(t *testing.T) {
	// A14.2：缓存超字节上限 → 旧 mtime 先驱逐至 90% 水位（ADR-225 F14）
	old := ReposDir
	ReposDir = t.TempDir()
	defer func() { ReposDir = old }()
	t.Setenv("CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES", "6000")
	root := rehydrateCacheRoot()
	// 三个任务缓存：t-old(5000B, 旧) / t-mid(5000B, 中) / t-new(5000B, 新) —— 总 15000 > 6000
	for _, c := range []struct {
		id  string
		age time.Duration
	}{
		{"t-old", 3 * time.Hour}, {"t-mid", 2 * time.Hour}, {"t-new", time.Hour},
	} {
		dir := filepath.Join(root, c.id, "unpacked")
		mkTreeFiles(t, dir, map[string]string{"blob.bin": strings.Repeat("x", 5000)})
		past := time.Now().Add(-c.age)
		_ = os.Chtimes(filepath.Join(root, c.id), past, past) // LRU 序键=<id> 父目录 mtime（与实现一致）
		_ = os.Chtimes(dir, past, past)
	}
	(&Transcoder{}).enforceRehydrateLRU()
	if _, err := os.Stat(filepath.Join(root, "t-old")); !os.IsNotExist(err) {
		t.Fatalf("最旧缓存未驱逐: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "t-new")); err != nil {
		t.Fatalf("最新缓存不应被驱逐: %v", err)
	}
}
