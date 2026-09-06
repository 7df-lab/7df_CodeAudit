// AI 交互日志（ADR-168）：沙箱内 DSH 经 bridge 的 SSE 交互原始帧留存。
// 双写内存环形缓冲（进程内实时增量读，GUI 运行中回显）+ 磁盘落盘（任务终态后
// 仍可整体获取——最终交互日志；进程重启后内存丢失，从文件兜底读取）。
// 容量上限 16MB/任务：超限停止追加并留截断标记（诚实留痕，不静默丢弃）。
package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	aiLogMaxBytes   = 16 << 20 // 单任务交互日志内存/磁盘上限（16MB）
	aiLogReadDefault = 256 << 10 // GetAIInteractionLog 单次默认返回 256KB
)

// aiLogEntry — 单任务的交互日志留存（人性化流）。
type aiLogEntry struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	complete  bool   // 分析已终态（Run 返回后置位）
	humanPath string // 人性化流落盘路径（重启兜底；空=未配置落盘）
	rawPath   string // 原始 SSE 帧落盘路径（机器调试留存，不经 RPC）
	subs      map[chan struct{}]bool // ADR-189 流式订阅者（write/finish 即时唤醒）
}

// aiLogStore — 任务级交互日志注册表。
type aiLogStore struct {
	mu   sync.Mutex
	logs map[string]*aiLogEntry
	dir  string // 落盘根目录（空=不落盘）
}

func newAILogStore(dir string) *aiLogStore {
	return &aiLogStore{logs: map[string]*aiLogEntry{}, dir: dir}
}

// writer — 取（或惰性建）任务日志条目。
func (s *aiLogStore) writer(taskID string) *aiLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.logs[taskID]
	if !ok {
		e = &aiLogEntry{subs: map[chan struct{}]bool{}}
		if s.dir != "" {
			e.humanPath = filepath.Join(s.dir, taskID+".ai.log")
			e.rawPath = filepath.Join(s.dir, taskID+".sse.log")
			_ = os.MkdirAll(s.dir, 0o755)
		}
		s.logs[taskID] = e
	}
	return e
}

// entry — 取（或惰性建）任务日志条目的等价写法（GetAIInteractionLog/Stream 共用）。
func (s *aiLogStore) entry(taskID string) *aiLogEntry {
	return s.writer(taskID)
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

// writeRaw — 原始 SSE 帧仅落盘（.sse.log，机器调试留存；不进内存/RC 面向用户流）。
func (e *aiLogEntry) writeRaw(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appendFile(e.rawPath, p)
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

// finish — 标记终态（最终交互日志就此定格）。
func (e *aiLogEntry) finish() {
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

// sharedAILogs — 进程级交互日志注册表（DSHRuntimeServiceImpl.aiLogs 的包级同源；
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
