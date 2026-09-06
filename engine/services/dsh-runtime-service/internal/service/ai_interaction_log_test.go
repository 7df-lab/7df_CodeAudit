// AI 交互日志留存单元回归（ADR-168）：游标增量读 / 终态定格 / 落盘兜底读 / 截断诚实留痕。
package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAILogEntry_CursorIncrementalRead(t *testing.T) {
	e := &aiLogEntry{}
	e.write([]byte("hello "))
	e.write([]byte("world"))
	chunk, next, complete, total := e.read(0, 0)
	if string(chunk) != "hello world" || next != 11 || complete || total != 11 {
		t.Fatalf("read1: chunk=%q next=%d complete=%v total=%d", chunk, next, complete, total)
	}
	chunk, next, complete, _ = e.read(6, 0)
	if string(chunk) != "world" || next != 11 || complete {
		t.Fatalf("read2: chunk=%q next=%d complete=%v", chunk, next, complete)
	}
	// 越界游标：返回空且游标不回退
	chunk, next, _, _ = e.read(99, 0)
	if chunk != nil || next != 99 {
		t.Fatalf("read3: chunk=%q next=%d", chunk, next)
	}
	// maxBytes 分片
	chunk, next, _, _ = e.read(0, 5)
	if string(chunk) != "hello" || next != 5 {
		t.Fatalf("read4: chunk=%q next=%d", chunk, next)
	}
	e.finish()
	if _, _, complete, _ := e.read(0, 0); !complete {
		t.Fatal("finish() must set complete")
	}
}

func TestAILogEntry_DiskSpillFallback(t *testing.T) {
	dir := t.TempDir()
	s := newAILogStore(dir)
	e := s.writer("task-x")
	e.write([]byte("── 第 1 轮开始 ──\n"))            // 人性化流 → 内存 + .ai.log
	e.write([]byte("💭 [思考]\n"))
	e.writeRaw([]byte("event: bridge.hello\n")) // 原始帧 → 仅 .sse.log
	if _, _, complete, _ := e.read(0, 0); complete {
		t.Fatal("in-flight entry must not be complete")
	}
	human, err := os.ReadFile(filepath.Join(dir, "task-x.ai.log"))
	if err != nil || !strings.Contains(string(human), "思考") {
		t.Fatalf("humanized disk spill: %v %.80s", err, human)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "task-x.sse.log"))
	if err != nil || !strings.Contains(string(raw), "bridge.hello") {
		t.Fatalf("raw disk spill: %v %.80s", err, raw)
	}
	if strings.Contains(string(human), "bridge.hello") {
		t.Fatal("humanized file must not contain raw frames")
	}
	// 进程重启形态：新 store 无内存条目 → 从 .ai.log 兜底读且视为已终态
	s2 := newAILogStore(dir)
	e2 := s2.writer("task-x")
	chunk, _, complete, total := e2.read(0, 0)
	if !complete || total == 0 || !strings.Contains(string(chunk), "思考") {
		t.Fatalf("disk fallback: complete=%v total=%d chunk=%.40s", complete, total, chunk)
	}
}

// TestAILogEntry_LegacyRawFallback — 早于人性化渲染的任务只有 .sse.log：如实回退原始帧。
func TestAILogEntry_LegacyRawFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.sse.log"), []byte("event: bridge.hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newAILogStore(dir)
	e := s.writer("legacy")
	chunk, _, complete, total := e.read(0, 0)
	if !complete || total == 0 || !strings.Contains(string(chunk), "bridge.hello") {
		t.Fatalf("legacy fallback: complete=%v total=%d chunk=%.40s", complete, total, chunk)
	}
}

func TestAILogEntry_TruncationHonest(t *testing.T) {
	e := &aiLogEntry{}
	big := strings.Repeat("x", aiLogMaxBytes) // 恰好装满
	e.write([]byte(big))
	e.write([]byte("overflow-after-cap"))
	if got := e.buf.String(); !strings.Contains(got, "truncated") || strings.Contains(got, "overflow-after-cap") {
		t.Fatalf("truncation marker missing or overflow leaked: len=%d tail=%.80s", len(got), got[len(got)-80:])
	}
}

func TestAILogStore_ConcurrentWriters(t *testing.T) {
	s := newAILogStore("")
	e := s.writer("task-c")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.write([]byte("chunk-data"))
		}()
	}
	wg.Wait()
	_, next, _, total := e.read(0, 0)
	if total != 80 || next != 80 {
		t.Fatalf("concurrent writes: total=%d next=%d", total, next)
	}
}
