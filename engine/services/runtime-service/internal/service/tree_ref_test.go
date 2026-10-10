package service

// tree_ref 快照测试（R3 v0，切片 1）：handoff.checkpointed.workspace.tree_ref 可被
// 离线解析回源码树——快照落盘 + sha256 内容寻址 + 解包内容一致。
// 依据: 主仓库 baton/docs/engine-integration-req.md R3 验收；ADR-244（tasklog 卷落点）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceTreeRef_OfflineResolvable(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "src", "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEAUDIT_TASKLOG_DIR", t.TempDir())

	ref := workspaceTreeRef("gw-tree1", proj)
	if !strings.HasPrefix(ref, "sha256:") || len(ref) != len("sha256:")+64 {
		t.Fatalf("ref = %q, want sha256:<64hex>", ref)
	}
	sum := strings.TrimPrefix(ref, "sha256:")

	archive := filepath.Join(tasklogDir(), "gw-tree1", "workspace", "tree-"+sum+".tar.gz")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("snapshot archive missing: %v", err)
	}
	if got := sha256.Sum256(raw); hex.EncodeToString(got[:]) != sum {
		t.Fatal("archive bytes do not match content address")
	}

	// 离线解析：解包内容回源码树（验收原文）。
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, _ := io.ReadAll(tr)
		files[hdr.Name] = string(data)
	}
	if files["src/a.go"] != "package a\n" || files["README.md"] != "# demo\n" {
		t.Fatalf("unpacked tree mismatch: %v", files)
	}
}

// 降级纪律：projectPath 空=无快照无引用（宁缺不错，不空锚）。
func TestWorkspaceTreeRef_EmptyProjectDegrades(t *testing.T) {
	t.Setenv("CODEAUDIT_TASKLOG_DIR", t.TempDir())
	if ref := workspaceTreeRef("gw-tree2", ""); ref != "" {
		t.Fatalf("empty project must degrade to empty ref, got %q", ref)
	}
}
