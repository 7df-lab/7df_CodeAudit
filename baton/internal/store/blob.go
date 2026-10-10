// Package store 内容寻址正文库（.tasklog/blobs/）。JSONL 行流只放引用，
// 大载荷正文进库——"裁剪与档案分离"的物理形态：旧输出可退出提示词但不离开档案。
package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type BlobStore struct {
	dir     string
	written map[string]bool
	count   int
}

func New(dir string) (*BlobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &BlobStore{dir: dir, written: map[string]bool{}}, nil
}

// Put 写入正文并返回引用（blob:<sha256 前 16 位>）。同内容去重；已存在不重写。
// 落盘走临时文件+rename：截断半文件若以完整 sha 之名留存，会被去重短路永久
// 信任——文件名即内容承诺，写入必须原子。Stat 非 NotExist 错误（权限等）上抛，
// 不静默跳过（引用指向不存在的正文比失败更糟）。
func (s *BlobStore) Put(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	hex := fmt.Sprintf("%x", sum)
	path := filepath.Join(s.dir, hex)
	if !s.written[hex] {
		if _, err := os.Stat(path); err == nil {
			s.written[hex] = true
		} else if errors.Is(err, os.ErrNotExist) {
			if err := WriteFileAtomic(path, b); err != nil {
				return "", err
			}
			s.count++
			s.written[hex] = true
		} else {
			return "", err
		}
	}
	return "blob:" + hex[:16], nil
}

// WriteFileAtomic — 临时文件+rename 原子落盘（读者要么见旧全文要么见新全文）。
// 导出共用了 project 投影器落盘面（2026-09-21 code-review 去重：两处同型内联
// 收敛单点——"截断半文件被去重短路永久信任"与"torn 档案丢 handoff 尾段"是同一
// 纪律的两个消费面）。
func WriteFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *BlobStore) Count() int { return s.count }
