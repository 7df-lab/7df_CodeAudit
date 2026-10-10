// AI 交互日志留存单元回归（ADR-168）：游标增量读 / 终态定格 / 落盘兜底读 / 截断诚实留痕。
package service

import (
	"bytes"
	"container/list"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	sandbox "github.com/codeaudit/services/runtime-service/internal/sandbox"
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
	e := s.writer("task-x", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.write([]byte("── 第 1 轮开始 ──\n")) // 人性化流 → 内存 + .ai.log
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
	e2 := s2.writer("task-x", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
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
	e := s.writer("legacy", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
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
	e := s.writer("task-c", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
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
	first := s.writer("task-000", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	first.write([]byte("oldest"))
	first.finish()
	// writer() 内联触发淘汰：填满上限再插入，最早的终态条目即被挤出
	for i := 1; i <= aiLogMaxEntries; i++ {
		e := s.writer(fmtTaskID(i), pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
		e.write([]byte("x"))
		e.finish()
	}
	if _, ok := s.logs["task-000"]; ok {
		t.Fatalf("oldest completed entry must be evicted inline at insert")
	}
	// 再插一条新任务 → 最老的终态条目继续被挤掉
	s.writer("task-new", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
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
	running := s.writer("task-running", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	running.write([]byte("in-flight"))
	// 全部其他条目置为终态且更老
	for i := 0; i < aiLogMaxEntries; i++ {
		e := s.writer(fmtTaskID(i), pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
		e.write([]byte("x"))
		e.finish()
	}
	// running 最老（最先创建）——若淘汰不区分终态，它应最先被挤掉
	s.lru.MoveToBack(running.lruEl.(*list.Element))
	s.writer("task-new", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	if _, ok := s.logs["task-running"]; !ok {
		t.Fatalf("in-flight entry must be protected while completed evictables exist")
	}
}

// ADR-215 回归：淘汰后读路径走磁盘兜底——数据不丢。
func TestAILogStore_EvictedTaskReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	s := newAILogStore(dir)
	e := s.writer("task-evict", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
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

// R36 回归锁（gw-d331089f 实证）：interaction_dir 的部署覆盖口——容器化后
// task-service 对账探针（ADR-196）与本服务各持 CWD 相对路径，互不可见 → 探针恒
// miss，长审计任务被误判 TIMEOUT。修复=CODEAUDIT_INTERACTION_DIR env 优先，
// compose 注入共享卷路径；yaml 相对路径仅本地开发缺省。
func TestInteractionDir_EnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "codeaudit.yaml")
	fixture := "runtime:\n  sandbox:\n    interaction_dir: \"data/ai-interaction-local\"\n"
	if err := os.WriteFile(cfgPath, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEAUDIT_CONFIG", cfgPath)
	t.Setenv("CODEAUDIT_INTERACTION_DIR", "/data/repos/ai-interaction")
	// 注：Default() 为 sync.Once 进程级缓存，此处不断言 yaml 缺省回退（属 go-config
	// 行为且受缓存序影响）——只锁"部署覆盖口必须生效"这一回归面。
	if got := interactionDir(); got != "/data/repos/ai-interaction" {
		t.Fatalf("env override must win: got %q", got)
	}
}

// ===== 图节点通道=====

// TestAIChannel_CursorIncrementalRead — 通道游标增量读（语义对齐主面 read）。
func TestAIChannel_CursorIncrementalRead(t *testing.T) {
	e := newAILogStore("").writer("t-ch", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.writeChannel("n001", []byte("第一段"))
	e.writeChannel("n002", []byte("另一节点"))
	chunk, next, _, total := e.readChannel("n001", 0, 1<<20)
	if string(chunk) != "第一段" || next != int64(len("第一段")) || total != next {
		t.Fatalf("首读失常: %q next=%d total=%d", chunk, next, total)
	}
	if c2, n2, _, _ := e.readChannel("n001", next, 1<<20); len(c2) != 0 || n2 != next {
		t.Fatalf("游标尽读应空: %q", c2)
	}
	// 节点隔离：n002 不受 n001 游标影响
	if c3, _, _, t3 := e.readChannel("n002", 0, 1<<20); string(c3) != "另一节点" || t3 != int64(len("另一节点")) {
		t.Fatalf("节点隔离破缺: %q", c3)
	}
}

// TestAIChannel_DiskFallback — 盘兜底：条目淘汰/重启后按文件读（存在即定格）。
func TestAIChannel_DiskFallback(t *testing.T) {
	dir := t.TempDir()
	s := newAILogStore(dir)
	e := s.writer("t-disk", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.writeChannel("n001", []byte("落盘内容"))
	// 新 store（模拟重启）：writer 惰性重建条目，内存通道空 → 文件兜底
	s2 := newAILogStore(dir)
	e2 := s2.writer("t-disk", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	chunk, next, complete, total := e2.readChannel("n001", 0, 1<<20)
	if string(chunk) != "落盘内容" || !complete || next != total {
		t.Fatalf("盘兜底失常: %q complete=%v", chunk, complete)
	}
}

// TestAIChannel_TruncationMarker — 超限截断留痕（对齐主面纪律）。
func TestAIChannel_TruncationMarker(t *testing.T) {
	e := newAILogStore("").writer("t-tr", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.writeChannel("n001", make([]byte, aiLogMaxBytes))
	e.writeChannel("n001", []byte("溢出段"))
	tail, _, _, total := e.readChannel("n001", int64(aiLogMaxBytes)-64, 1<<20) // 尾窗读（标记在末尾）
	if total > aiLogMaxBytes+128 {                                             // 截断标记行余量（中文标记 ~81B）
		t.Fatalf("截断失效: total=%d", total)
	}
	if !bytes.Contains(tail, []byte("ai-channel truncated")) {
		t.Fatalf("截断标记缺失: %q", tail)
	}
	// 其它节点不受截断影响
	e.writeChannel("n002", []byte("正常"))
	if c, _, _, _ := e.readChannel("n002", 0, 1<<20); string(c) != "正常" {
		t.Fatalf("n002 被连带: %q", c)
	}
}

// TestAIChannel_NodeIDHardening — 路径分隔符拒绝（文件名注入面）。
func TestAIChannel_NodeIDHardening(t *testing.T) {
	dir := t.TempDir()
	e := newAILogStore(dir).writer("t-inj", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.writeChannel("../escape", []byte("x"))
	e.writeChannel("a/b", []byte("x"))
	if _, _, _, total := e.readChannel("../escape", 0, 1<<20); total != 0 {
		t.Fatal("路径逃逸通道被创建")
	}
	if _, err := os.Stat(filepath.Join(dir, "t-inj.n-..escape.ai.log")); err == nil {
		t.Fatal("逃逸文件落盘")
	}
}

// TestAIChannel_EmptyNodeIDNoDeadlock — 空 nodeID 早退不得持锁（P1 回归锁：
// 一次 readChannel("") 后 write 必须仍能推进——审查批 2026-09-23 抓出的死锁）。
func TestAIChannel_EmptyNodeIDNoDeadlock(t *testing.T) {
	e := newAILogStore("").writer("t-nil", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	done := make(chan struct{})
	go func() {
		e.readChannel("", 0, 1<<20)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readChannel(\"\") 挂死——早退路径持锁未放")
	}
	e.writeChannel("n001", []byte("后续写不受影响")) // 若锁未放，此处永久阻塞 → 测试超时红
	if c, _, _, _ := e.readChannel("n001", 0, 1<<20); string(c) != "后续写不受影响" {
		t.Fatalf("后续读写失常: %q", c)
	}
}

// TestChannelDeltas_EnumerationAndCursors — 通道枚举（内存∪磁盘）与游标增量
// （批2 服务面：Get/Stream 的数据源）。
func TestChannelDeltas_EnumerationAndCursors(t *testing.T) {
	dir := t.TempDir()
	s := newAILogStore(dir)
	e := s.writer("t-d2", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.writeChannel("n002", []byte("乙节点"))
	e.writeChannel("n001", []byte("甲节点第一段"))
	// 游标增量：n001 从 0 读到全部，再从 next 读应空
	ds := s.channelDeltas("t-d2", nil, 1<<20)
	if len(ds) != 2 || ds[0].NodeID != "n001" || ds[1].NodeID != "n002" { // 字母序稳定
		t.Fatalf("枚举失常: %+v", ds)
	}
	cur := map[string]int64{"n001": ds[0].Next}
	ds2 := s.channelDeltas("t-d2", cur, 1<<20)
	var n001 *aiChannelDelta
	for i := range ds2 {
		if ds2[i].NodeID == "n001" {
			n001 = &ds2[i]
		}
	}
	if n001 == nil || len(n001.Chunk) != 0 || n001.Total == 0 {
		t.Fatalf("已读游标应零增量但保留 total: %+v", n001)
	}
	// 重启场景：新 store 无条目 → 磁盘 glob 枚举
	s3 := newAILogStore(dir)
	ds3 := s3.channelDeltas("t-d2", nil, 1<<20)
	if len(ds3) != 2 {
		t.Fatalf("盘枚举失常: %+v", ds3)
	}
}

// TestStreamAIChannels_DrainsOversizeAndSettles — 流式活性锁（审查 P1-2）：
// 已完结任务 + 通道总量 > maxBytes → 订阅流必须自我排干收束（不等新写）。
func TestStreamAIChannels_DrainsOversizeAndSettles(t *testing.T) {
	dir := t.TempDir()
	impl := &RuntimeServiceImpl{aiLogs: newAILogStore(dir)}
	e := impl.aiLogs.writer("t-big", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	big := strings.Repeat("字", 40_000) // 120KB > maxBytes=64KB → 两轮排干
	e.writeChannel("n001", []byte(big))
	e.finish() // 任务已收束（订阅前 finish 信号已耗尽——P1-2 的死等形态）

	// 直接驱动 Stream 主循环的等价形态：反复 channelDeltas 推进至排干
	cursors := map[string]int64{}
	rounds := 0
	for rounds < 10 {
		ds := impl.aiLogs.channelDeltas("t-big", cursors, 64<<10)
		drained := true
		progress := false
		for _, d := range ds {
			if len(d.Chunk) > 0 {
				progress = true
			}
			cursors[d.NodeID] = d.Next
			if d.Next < d.Total {
				drained = false
			}
		}
		if !progress {
			break // 无新 chunk：模拟 P1-2 的 notify 永不到达——此时必须已 drained
		}
		if drained {
			break
		}
		rounds++
	}
	if cursors["n001"] != int64(len(big)) {
		t.Fatalf("排干失效: cursor=%d total=%d rounds=%d（P1-2 死等回归）", cursors["n001"], len(big), rounds)
	}
}
