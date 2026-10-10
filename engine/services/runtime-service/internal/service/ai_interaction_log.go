// AI 交互日志（ADR-168）：沙箱内 DSH 经 bridge 的 SSE 交互原始帧留存。
// 双写内存环形缓冲（进程内实时增量读，GUI 运行中回显）+ 磁盘落盘（任务终态后
// 仍可整体获取——最终交互日志；进程重启后内存丢失，从文件兜底读取）。
// 容量上限 16MB/任务：超限停止追加并留截断标记（诚实留痕，不静默丢弃）。
package service

import (
	"bytes"
	"container/list"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

const (
	aiLogMaxBytes    = 16 << 20  // 单任务交互日志内存/磁盘上限（16MB）
	aiLogReadDefault = 256 << 10 // GetAIInteractionLog 单次默认返回 256KB
	// ADR-215: 进程级双上限——条目数与总字节。此前 map 只增不减（线性内存泄漏，
	// 数百任务即 GB 级驻留）；磁盘 .ai.log/.sse.log 兜底使内存淘汰不丢数据
	// （读路径 readDiskOnly 承接）。数值属实现细节，依据见 ADR-215。
	aiLogMaxEntries    = 64
	aiLogMaxTotalBytes = 256 << 20
)

// aiLogEntry — 单任务的交互日志留存（人性化流）。
type aiLogEntry struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	truncated   bool
	complete    bool                   // 分析已终态（Run 返回后置位）
	humanPath   string                 // 人性化流落盘路径（重启兜底；空=未配置落盘）
	rawPath     string                 // 原始 SSE 帧落盘路径（机器调试留存，不经 RPC）
	graphPath   string                 // 图事件流落盘路径（切片 5+6 ADR-249：<task>.graph.log；空=未配置）
	channels    map[string]*aiChannel  // 图节点通道（graph-ui-channels 批1a，2026-09-23）：nodeID → 逐节点流
	granularity pb.AILogGranularity    // b4/D2：到达粒度（创建时按方言初定，wireAILog 每次接线随当前方言刷新——运行期可变，读必经 granularityNow 持锁；dsh-sse=逐字；acp-stdio/maka=整回合）
	subs        map[chan struct{}]bool // ADR-189 流式订阅者（write/finish 即时唤醒）
	taskID      string                 // ADR-215: 淘汰定位用
	lruEl       any                    // ADR-215: *list.Element（store LRU 位）
}

// aiChannel — 图任务单节点通道（graph-ui-channels 批1a）：与主 humanized 流同纪律的
// 追加+游标读（内存=RPC 源；磁盘 <task>.n-<nodeID>.ai.log 兜底；16MB/通道截断留痕）。
// complete 语义共享所属条目（任务收束即全部通道定格——节点级完成位由 /graph 状态面
// 表达，通道只管内容）。批2 契约（ai_channels RPC 面）以 readChannel 为数据源。
type aiChannel struct {
	buf       bytes.Buffer
	truncated bool
	path      string
}

// aiLogStore — 任务级交互日志注册表（ADR-215：LRU 有界，淘汰序=最久未触碰，
// 优先淘汰已终态条目；进行中（未 finish）条目只在无终态条目可淘汰时才按
// 内存上限硬性淘汰——磁盘副本兜底，数据不丢）。
type aiLogStore struct {
	mu   sync.Mutex
	logs map[string]*aiLogEntry
	lru  *list.List // 前端=最近触碰；元素 *aiLogEntry
	dir  string     // 落盘根目录（空=不落盘）
}

func newAILogStore(dir string) *aiLogStore {
	return &aiLogStore{logs: map[string]*aiLogEntry{}, lru: list.New(), dir: dir}
}

// touchLocked — 刷新 LRU 位（调用方持 s.mu）。
func (s *aiLogStore) touchLocked(e *aiLogEntry) {
	if el, ok := e.lruEl.(*list.Element); ok {
		s.lru.MoveToFront(el)
	}
}

// evictLocked — 超上限时从尾部（最久未触碰）淘汰：先淘汰已终态条目；
// 仍超限（极端：全部进行中）再按序硬淘汰，内存上限优先。
func (s *aiLogStore) evictLocked() {
	over := func() bool {
		if len(s.logs) <= aiLogMaxEntries && s.totalBytesLocked() <= aiLogMaxTotalBytes {
			return false
		}
		return len(s.logs) > 0
	}
	for pass := 0; pass < 2 && over(); pass++ {
		for el := s.lru.Back(); el != nil && over(); {
			prev := el.Prev()
			cand := el.Value.(*aiLogEntry)
			if pass == 0 && !cand.complete { // 第一轮只淘汰已终态
				el = prev
				continue
			}
			s.lru.Remove(el)
			delete(s.logs, cand.taskID)
			el = prev
		}
	}
}

// totalBytesLocked — 全条目内存占用合计（条目数 ≤ 上限，遍历廉价；调用方持 s.mu）。
func (s *aiLogStore) totalBytesLocked() int64 {
	var total int64
	for _, e := range s.logs {
		e.mu.Lock()
		total += int64(e.buf.Len())
		for _, ch := range e.channels { // graph-ui-channels：通道缓冲同入账（防 24×16MB 级逃账）
			total += int64(ch.buf.Len())
		}
		e.mu.Unlock()
	}
	return total
}

// writer — 取（或惰性建）任务日志条目；新建即触发 LRU 淘汰。
func (s *aiLogStore) writer(taskID string, granularity pb.AILogGranularity) *aiLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.logs[taskID]
	if ok {
		s.touchLocked(e)
		return e
	}
	e = &aiLogEntry{subs: map[chan struct{}]bool{}, taskID: taskID, granularity: granularity}
	if s.dir != "" {
		e.humanPath = filepath.Join(s.dir, taskID+".ai.log")
		e.rawPath = filepath.Join(s.dir, taskID+".sse.log")
		e.graphPath = filepath.Join(s.dir, taskID+".graph.log") // ADR-249：与 .sse.log 同目录同纪律
		_ = os.MkdirAll(s.dir, 0o755)
	}
	s.logs[taskID] = e
	e.lruEl = s.lru.PushFront(e)
	s.evictLocked()
	return e
}

// granularityNow — 粒度并发安全读（对抗式审查 P2-5：D2 换笔刷新使该字段运行期
// 可变，Get/Stream 组装处裸读=数据竞争（-race 判杀纪律 M56））。
func (e *aiLogEntry) granularityNow() pb.AILogGranularity {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.granularity
}

// granularityOf — 方言 → 到达粒度映射（b4 契约真源：粒度是协议固有属性——
// dsh-sse 逐字 chunk=TOKEN；acp-stdio 管道/maka runtime-host 回合到达=TURN；
// 未知/空=UNSPECIFIED，前端回落 agent 判别过渡案）。
func granularityOf(dialect string) pb.AILogGranularity {
	switch dialect {
	case "dsh-sse", "mock-stdio":
		return pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN
	case "acp-stdio", "maka-runtime-host":
		return pb.AILogGranularity_AI_LOG_GRANULARITY_TURN
	default:
		return pb.AILogGranularity_AI_LOG_GRANULARITY_UNSPECIFIED
	}
}

// subscribe — 注册流式订阅者（ADR-189），返回通知通道与退订函数。
func (e *aiLogEntry) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1) // 1 缓冲：合并连续写，唤醒后按游标读全量增量
	e.mu.Lock()
	if e.subs == nil {
		e.subs = map[chan struct{}]bool{}
	}
	e.subs[ch] = true
	e.mu.Unlock()
	unsub := func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
	return ch, unsub
}

// notifyLocked — 非阻塞唤醒全部订阅者（调用方持 e.mu；不阻塞写入主链路）。
func (e *aiLogEntry) notifyLocked() {
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default: // 已有挂起信号：订阅者醒来会读到全部增量
		}
	}
}

// write — 追加人性化流字节（内存=RPC 数据源；.ai.log 落盘兜底）。
func (e *aiLogEntry) write(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.truncated {
		return
	}
	if e.buf.Len()+len(p) > aiLogMaxBytes {
		e.truncated = true
		marker := fmt.Appendf(nil, "\n[ai-log truncated: 超过 %d 字节上限，后续内容不再追加]\n", int64(aiLogMaxBytes))
		e.buf.Write(marker)
		e.appendFile(e.humanPath, marker)
		e.notifyLocked() // ADR-189：截断标记同样要到达订阅端
		return
	}
	e.buf.Write(p)
	e.appendFile(e.humanPath, p)
	e.notifyLocked() // ADR-189：写入即唤醒订阅流
}

// writeChannel — 追加节点通道字节（graph-ui-channels 批1a）：纪律同 write
// （16MB/通道截断留痕；磁盘兜底；写入即唤醒订阅流）。nodeID 由引擎生成
// （n001 型短 id），防御性拒绝路径分隔符（文件名注入面）。
func (e *aiLogEntry) writeChannel(nodeID string, p []byte) {
	if nodeID == "" || strings.ContainsAny(nodeID, `/\`) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.channels == nil {
		e.channels = map[string]*aiChannel{}
	}
	ch := e.channels[nodeID]
	if ch == nil {
		ch = &aiChannel{path: channelPath(e.humanPath, nodeID)}
		e.channels[nodeID] = ch
	}
	if ch.truncated {
		return
	}
	if ch.buf.Len()+len(p) > aiLogMaxBytes {
		ch.truncated = true
		marker := fmt.Appendf(nil, "\n[ai-channel truncated: 超过 %d 字节上限，后续内容不再追加]\n", int64(aiLogMaxBytes))
		ch.buf.Write(marker)
		e.appendFile(ch.path, marker)
		e.notifyLocked()
		return
	}
	ch.buf.Write(p)
	e.appendFile(ch.path, p)
	e.notifyLocked()
}

// channelPath — 主 humanized 落盘路径 → 同目录节点通道路径（<task>.n-<nodeID>.ai.log）。
// humanPath 为空（禁落盘态）返回空——内存态照常工作。
func channelPath(humanPath, nodeID string) string {
	if humanPath == "" {
		return ""
	}
	return strings.TrimSuffix(humanPath, ".ai.log") + ".n-" + nodeID + ".ai.log"
}

// readChannel — 节点通道游标增量读（批2 契约数据源）：内存优先；内存空且磁盘有档
// 则兜底（进程重启/条目淘汰场景，文件存在即视为已定格）。语义对齐 read
// （调用方持锁约定同 read——本方法自持 e.mu）。
func (e *aiLogEntry) readChannel(nodeID string, cursor int64, maxBytes int32) (chunk []byte, next int64, complete bool, total int64) {
	if cursor < 0 {
		cursor = 0 // 审查 P2-4：负游标钳零（内网直连面可达；负下标切片 panic）
	}
	if nodeID == "" {
		complete := func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.complete }()
		return nil, cursor, complete, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.channels[nodeID]
	if ch == nil || ch.buf.Len() == 0 {
		if path := channelPath(e.humanPath, nodeID); path != "" {
			if raw, err := os.ReadFile(path); err == nil {
				total = int64(len(raw))
				if cursor >= total {
					return nil, total, true, total
				}
				end := total
				if maxBytes > 0 && end-cursor > int64(maxBytes) {
					end = cursor + int64(maxBytes)
				}
				return raw[cursor:end], end, true, total
			}
		}
		return nil, cursor, e.complete, 0
	}
	total = int64(ch.buf.Len())
	if cursor >= total {
		return nil, cursor, e.complete, total
	}
	end := total
	if maxBytes > 0 && end-cursor > int64(maxBytes) {
		end = cursor + int64(maxBytes)
	}
	return append([]byte(nil), ch.buf.Bytes()[cursor:end]...), end, e.complete, total
}

// writeRaw — 原始 SSE 帧仅落盘（.sse.log，机器调试留存；不进内存/RC 面向用户流）。
// R5（TP17-T3）：onRaw 逐行外流，帧首（event: 行）前缀 `: ts <RFC3339Nano>` 注释
// =帧接收时刻——SSE 合法注释，旧解析器（engine sseParser/baton 旧版）天然跳过，
// 新旧读写双向兼容；baton 投影器据此把事件 ts 从投影时刻升级为发生时刻。
func (e *aiLogEntry) writeRaw(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if bytes.HasPrefix(p, []byte("event: ")) {
		tsLine := append([]byte(": ts "), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n")...)
		e.appendFile(e.rawPath, append(tsLine, p...)) // 注释+帧首一次写（免二次 open）
		return
	}
	e.appendFile(e.rawPath, p)
}

// writeGraph — 图事件 JSONL 行落盘（<task>.graph.log，ADR-249：graph 状态的唯一
// 持久真相，A1）。每行自带换行（graphEmitter 产出）；失败静默——留存通道不得影响
// 执行主链路（同 writeRaw 纪律）。resume 输入=readGraphLog。
// R116：与 writeRaw 分道——graph 行是 resume 的唯一输入，短写/页缓存丢失=撕裂行
// （读侧 R116 起容忍丢弃，但丢事件=节点状态分叉），故全量写校验+Sync；图事件
// 低频（每节点数条），fsync 成本可接受。
func (e *aiLogEntry) writeGraph(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appendGraphFile(e.graphPath, p)
}

// appendGraphFile — 磁盘追加写（调用方持锁；全量校验+Sync——graph.log 是 resume
// 唯一输入，撕裂成本高于 .sse.log；失败静默同留存纪律）。
func (e *aiLogEntry) appendGraphFile(path string, p []byte) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	n, werr := f.Write(p)
	if werr == nil && n < len(p) {
		werr = io.ErrShortWrite
	}
	if werr != nil {
		log.Printf("[graph-log] 追加写不完整（%d/%d）: %v", n, len(p), werr)
		return
	}
	if serr := f.Sync(); serr != nil {
		log.Printf("[graph-log] sync 失败: %v", serr)
	}
}

// graphLogReader — 既有 graph.log 的读取句柄（resume 回放输入；不存在=nil）。
func (e *aiLogEntry) graphLogReader() *os.File {
	if e.graphPath == "" {
		return nil
	}
	f, err := os.Open(e.graphPath)
	if err != nil {
		return nil // 无旧档=新任务
	}
	return f
}

// appendFile — 磁盘追加写（调用方持锁；失败静默——落盘是尽力而为的留存）。
func (e *aiLogEntry) appendFile(path string, p []byte) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(p)
}

// finish — 标记终态（最终交互日志就此定格）。nil 安全：禁用态不建条目
// （ADR-215），调用方的 defer finish() 无需判空。
func (e *aiLogEntry) finish() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.complete = true
	e.notifyLocked() // ADR-189：终帧即时送达订阅端
}

// read — 游标增量读：返回 >cursor 的字节（至多 maxBytes），以及新游标/终态/总长。
// 内存无此任务时从落盘文件兜底读（进程重启场景；文件存在即视为已终态）。
func (e *aiLogEntry) read(cursor int64, maxBytes int32) (chunk []byte, next int64, complete bool, total int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.buf.Len() == 0 && e.humanPath != "" {
		raw, err := os.ReadFile(e.humanPath)
		if err != nil && e.rawPath != "" {
			// 早于人性化渲染的遗留任务只有 .sse.log——如实回退原始帧（数据完整性优先）
			raw, err = os.ReadFile(e.rawPath)
		}
		if err == nil {
			if int64(len(raw)) > cursor {
				end := int64(len(raw))
				if maxBytes > 0 && end-cursor > int64(maxBytes) {
					end = cursor + int64(maxBytes)
				}
				return raw[cursor:end], end, true, int64(len(raw))
			}
			return nil, cursor, true, int64(len(raw))
		}
		return nil, cursor, e.complete, int64(e.buf.Len())
	}
	total = int64(e.buf.Len())
	if cursor >= total {
		return nil, cursor, e.complete, total
	}
	end := total
	if maxBytes > 0 && end-cursor > int64(maxBytes) {
		end = cursor + int64(maxBytes)
	}
	return append([]byte(nil), e.buf.Bytes()[cursor:end]...), end, e.complete, total
}

// sharedAILogs — 进程级交互日志注册表（RuntimeServiceImpl.aiLogs 的包级同源；
// analyzeViaSandbox 不经 impl 实例（既有接线形态），故以包级单例承接）。
var sharedAILogs = newAILogStore(interactionDir())

// readDiskOnly — 无内存条目时的纯磁盘读（进程重启兜底）：文件存在即视为已终态。
func (s *aiLogStore) readDiskOnly(taskID string, cursor int64, maxBytes int32) (chunk []byte, next int64, total int64) {
	if s.dir == "" {
		return nil, cursor, 0
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, taskID+".ai.log"))
	if err != nil {
		// 遗留任务回退原始帧文件（早于人性化渲染）
		raw, err = os.ReadFile(filepath.Join(s.dir, taskID+".sse.log"))
		if err != nil {
			return nil, cursor, 0
		}
	}
	total = int64(len(raw))
	if cursor >= total {
		return nil, total, total
	}
	end := total
	if maxBytes > 0 && end-cursor > int64(maxBytes) {
		end = cursor + int64(maxBytes)
	}
	return raw[cursor:end], end, total
}

// ===== graph-ui-channels 批2：通道的服务面枚举与增量组装 =====

// aiChannelDelta — 单节点通道增量（pb AILogChannel 的仓内形状；pb 组装在 dsh_agent）。
type aiChannelDelta struct {
	NodeID   string
	Chunk    []byte
	Next     int64
	Complete bool
	Total    int64
}

// channelNodes — 该任务通道节点清单（内存 channels ∪ 磁盘 glob；字母序稳定输出）。
// 磁盘面=进程重启/条目淘汰后的兜底枚举（<task>.n-<nodeID>.ai.log 命名约定）。
func (e *aiLogEntry) channelNodes() []string {
	set := map[string]bool{}
	e.mu.Lock()
	for id := range e.channels {
		set[id] = true
	}
	human := e.humanPath
	e.mu.Unlock()
	// P2-3：内存通道非空时免 glob（内存 ⊆ 磁盘；glob 只服务重启/淘汰兜底，
	// Stream 每次唤醒全量目录扫描不可接受）。
	if len(set) == 0 && human != "" {
		if ms, err := filepath.Glob(strings.TrimSuffix(human, ".ai.log") + ".n-*.ai.log"); err == nil {
			for _, m := range ms {
				base := filepath.Base(m)
				if i := strings.Index(base, ".n-"); i >= 0 {
					set[strings.TrimSuffix(base[i+3:], ".ai.log")] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// channelDeltas — 组装各节点通道增量（游标缺省 0=从头；无内容节点不出场——
// 非图任务恒空切片）。既有条目直接读；无条目（重启场景）用一次性条目挂盘路径
// 读（不注册进 store——保持 Get 的"未知任务不建条目"纪律）。
func (s *aiLogStore) channelDeltas(taskID string, cursors map[string]int64, maxBytes int32) []aiChannelDelta {
	s.mu.Lock()
	e := s.logs[taskID]
	dir := s.dir
	s.mu.Unlock()
	if e == nil {
		if dir == "" {
			return nil
		}
		e = &aiLogEntry{humanPath: filepath.Join(dir, taskID+".ai.log")} // 一次性：仅盘路径，channels 空
	}
	var out []aiChannelDelta
	for _, n := range e.channelNodes() {
		chunk, next, complete, total := e.readChannel(n, cursors[n], maxBytes)
		if len(chunk) == 0 && total == 0 {
			continue
		}
		out = append(out, aiChannelDelta{NodeID: n, Chunk: chunk, Next: next, Complete: complete, Total: total})
	}
	return out
}
