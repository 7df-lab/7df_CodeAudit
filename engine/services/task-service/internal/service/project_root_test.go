package service

import (
	"os"
	"path/filepath"
	"testing"
)

// ResolveProjectRoot 回归锁（gw-f6a3523 实证）：压缩包顶层壳目录不剥时，
// fixpatch 校验/source-file 解析/沙箱视角三方根错位——7/7 补丁被误杀、
// 发现详情源码全文 404。剥壳语义 = "唯一子目录则降入"，封顶 3 层。

func writeTree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if filepath.Ext(abs) == "" || p[len(p)-1] == '/' {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveProjectRoot(t *testing.T) {
	cases := []struct {
		name string
		tree []string // 相对临时根的布局；目录以 / 结尾
		want string    // 相对临时根的期望结果；"" = 原样返回
	}{
		{"GitHub 式单壳目录", []string{"mica-mqtt-master/", "mica-mqtt-master/pom.xml", "mica-mqtt-master/src/App.java"}, "mica-mqtt-master"},
		{"双层壳逐层降入", []string{"outer/", "outer/inner/", "outer/inner/a.py"}, "outer/inner"},
		{"无壳多条目不降入", []string{"src/", "src/a.go", "README.md"}, ""},
		{"空目录原样返回", []string{}, ""},
		{"唯一条目是文件不降入", []string{"only.java"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.tree...)
			paths := make([]string, 0, len(tc.tree))
			for _, p := range tc.tree {
				paths = append(paths, p)
			}
			_ = paths
			got, err := filepath.Rel(root, ResolveProjectRoot(root))
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = "."
			}
			if got != want {
				t.Fatalf("ResolveProjectRoot = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveProjectRootDescentCap(t *testing.T) {
	// 深于封顶层数的纯目录链：降满 3 层即停，不无限下钻
	deep := writeTree(t, "a/", "a/b/", "a/b/c/", "a/b/c/d/", "a/b/c/d/f.txt")
	got := ResolveProjectRoot(deep)
	if filepath.Base(got) != "c" {
		t.Fatalf("降入封顶失效: got %q, want .../c", got)
	}
}
