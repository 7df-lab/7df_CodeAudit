// AI 交互日志留存单元回归（ADR-168）：游标增量读 / 终态定格 / 落盘兜底读 / 截断诚实留痕。
package service

import (
	"container/list"
	"os"
	"path/filepath"
	"strconv"

	sandbox "github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
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

// ADR-215 回归：LRU 双上限淘汰——条目数超限先淘汰最久未触碰的已终态条目
// （进行中条目保护），淘汰后读路径走磁盘兜底不丢数据。
func TestAILogStore_LRUEviction(t *testing.T) {
	s := newAILogStore("")
	// 填满上限（终态）+ 最老的条目置尾
	first := s.writer("task-000")
	first.write([]byte("oldest"))
	first.finish()
	// writer() 内联触发淘汰：填满上限再插入，最早的终态条目即被挤出
	for i := 1; i <= aiLogMaxEntries; i++ {
		e := s.writer(fmtTaskID(i))
		e.write([]byte("x"))
		e.finish()
	}
	if _, ok := s.logs["task-000"]; ok {
		t.Fatalf("oldest completed entry must be evicted inline at insert")
	}
	// 再插一条新任务 → 最老的终态条目继续被挤掉
	s.writer("task-new")
	if len(s.logs) > aiLogMaxEntries {
		t.Fatalf("store must stay within cap after insert, got %d", len(s.logs))
	}
	if _, ok := s.logs["task-new"]; !ok {
		t.Fatalf("newest entry must survive")
	}
	if len(s.logs) > aiLogMaxEntries {
		t.Fatalf("store must be within cap, got %d", len(s.logs))
	}
}

// ADR-215 回归：进行中（未 finish）条目受保护——第一轮只淘汰终态条目。
func TestAILogStore_IncompleteEntriesProtected(t *testing.T) {
	s := newAILogStore("")
	running := s.writer("task-running")
	running.write([]byte("in-flight"))
	// 全部其他条目置为终态且更老
	for i := 0; i < aiLogMaxEntries; i++ {
		e := s.writer(fmtTaskID(i))
		e.write([]byte("x"))
		e.finish()
	}
	// running 最老（最先创建）——若淘汰不区分终态，它应最先被挤掉
	s.lru.MoveToBack(running.lruEl.(*list.Element))
	s.writer("task-new")
	if _, ok := s.logs["task-running"]; !ok {
		t.Fatalf("in-flight entry must be protected while completed evictables exist")
	}
}

// ADR-215 回归：淘汰后读路径走磁盘兜底——数据不丢。
func TestAILogStore_EvictedTaskReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	s := newAILogStore(dir)
	e := s.writer("task-evict")
	e.write([]byte("precious payload"))
	e.finish()
	if _, ok := s.logs["task-evict"]; !ok {
		t.Fatalf("precondition: entry present")
	}
	// 强制淘汰：手动从注册表移除（模拟 LRU 挤出）
	s.mu.Lock()
	delete(s.logs, "task-evict")
	s.mu.Unlock()
	chunk, next, total := s.readDiskOnly("task-evict", 0, 0)
	const want = int64(len("precious payload"))
	if string(chunk) != "precious payload" || total != want || next != want {
		t.Fatalf("disk fallback after eviction: chunk=%q total=%d", chunk, total)
	}
}

func fmtTaskID(i int) string { return "task-" + string(rune('a'+i%26)) + fmtInt(i) }

func fmtInt(i int) string { return strconv.Itoa(i) }

// ADR-215 补记回归：wireAILog 必须在 runner 构造前把回调写进 cfg（runner 拷贝
// cfg，构造后补线即丢失——GUI 实测 AI 交互日志恒 0KB 的回归根因）；禁用态
// 不建条目且回调保持 nil。
func TestWireAILog_WiresCallbacksBeforeRunnerCopy(t *testing.T) {
	cfg := &sandbox.Config{Mode: "openshell"}
	e := wireAILog(cfg, "t-wire")
	if e == nil || cfg.OnHumanLog == nil || cfg.OnRawLog == nil {
		t.Fatalf("enabled mode must create entry and wire both callbacks")
	}
	cfg.OnHumanLog("hello") // 经 cfg 回调写入条目（拷贝后仍生效）
	chunk, _, _, _ := e.read(0, 0)
	if string(chunk) != "hello" {
		t.Fatalf("write via cfg callback missing: %q", chunk)
	}
}

func TestWireAILog_DisabledModeNoEntry(t *testing.T) {
	before := len(sharedAILogs.logs)
	cfg := &sandbox.Config{Mode: "rule"} // 非沙箱模式
	if e := wireAILog(cfg, "t-disabled"); e != nil {
		t.Fatalf("disabled mode must not create entry")
	}
	if cfg.OnHumanLog != nil || cfg.OnRawLog != nil {
		t.Fatalf("disabled mode must leave callbacks nil")
	}
	if len(sharedAILogs.logs) != before {
		t.Fatalf("disabled mode leaked an entry")
	}
}
