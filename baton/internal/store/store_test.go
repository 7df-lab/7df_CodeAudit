package store

// 评审缺口 G2 修复：BlobStore 直接锁定（此前仅经 project 幂等链间接覆盖）。
// 契约=internal-interfaces.md §I2：引用形状 blob:<sha16>、同内容去重不重写、
// Count=本次进程新建数。

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestBlobStore_PutDedupAndRefShape(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^blob:[0-9a-f]{16}$`).MatchString(r1) {
		t.Fatalf("引用形状 = %q（§I2: blob:<sha256 前 16 位>）", r1)
	}
	r2, _ := s.Put([]byte("hello")) // 同内容去重
	if r1 != r2 || s.Count() != 1 {
		t.Fatalf("去重: r1=%q r2=%q count=%d", r1, r2, s.Count())
	}
	if _, err := s.Put([]byte("world")); err != nil || s.Count() != 2 {
		t.Fatalf("新增计数 = %d", s.Count())
	}
	// 正文文件=全 sha256 名
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("blobs 目录文件数 = %d", len(entries))
	}
	for _, e := range entries {
		if len(e.Name()) != 64 {
			t.Fatalf("正文文件名须为全长 sha256: %q", e.Name())
		}
	}
}

func TestBlobStore_ReopenShortCircuit(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	ref, _ := s1.Put([]byte("payload"))
	// 模拟文件由前进程写入、新进程 map 空：不重写文件、Count 只计新建
	s2, _ := New(filepath.Join(dir)) // 同目录新实例（written map 空）
	ref2, err := s2.Put([]byte("payload"))
	if err != nil || ref != ref2 {
		t.Fatalf("重开去重: %q vs %q err=%v", ref, ref2, err)
	}
	if s2.Count() != 0 {
		t.Fatalf("已存在文件不应计新建: %d", s2.Count())
	}
}
