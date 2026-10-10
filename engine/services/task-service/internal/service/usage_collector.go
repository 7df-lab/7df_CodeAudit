// Package service — usage 计量归集（ADR-247 切片3后半；公理 D2：不可回补观测
// 必须终态即归集——.tasklog/events.jsonl 可被清理/重写，任务终态时点快照入库）。
//
// 通道裁决（ADR-247）：task-service 终态异步钩子直读共享卷 events.jsonl——
// tasklog 与 .sse.log 同卷（ADR-237）=agent_repos 四方共享卷，task 容器本就挂载；
// 文件读取是架构 §2 钦定的 baton 接缝（文件+CLI）。对比否决：RunResponse 带回
// =失败任务计量永失（最需计量的恰是失败面）；runtime 直写 PG=凭空多有状态
// 服务；新 gRPC 上报=环依赖。
//
// 幂等：baton project 对 events.jsonl 全量重写（ADR-239），本归集同构全量重算
// （事务 DELETE+INSERT），终态钩子重入/重试重启再触发均安全。
package service

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
)

// usageEnvelope — baton events.jsonl 信封（baton/internal/model/event.go 同形；
// 只取归集所需字段，未知字段忽略——schema 演进容错）。
type usageEnvelope struct {
	Seq     int64         `json:"seq"`
	Ts      string        `json:"ts"`
	Task    string        `json:"task"`
	Tenant  string        `json:"tenant"`
	User    string        `json:"user"`
	Session string        `json:"session"`
	Type    string        `json:"type"`
	Usage   *usagePayload `json:"usage,omitempty"`
}

// usagePayload — usage.recorded 载荷（baton Usage 同形：字段对齐 runtime
// TokenUsage；input=未缓存输入，计费输入=input+cache_read+cache_write）。
type usagePayload struct {
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
}

// tasklogDirForUsage — 与 runtime baton_hook.tasklogDir 同源派生（共享 ro 挂载
// 的 codeaudit.yaml + 同 env 约定）：tasklog_dir 显式配置 > <interaction_dir>/../tasklog。
// 空串=tasklog 未配置（归集禁用档，同 BATON_BIN 缺失语义）。
func tasklogDirForUsage() string {
	cfg, err := codeauditcfg.Default()
	if err == nil {
		if v, err := cfg.Str("runtime.sandbox.tasklog_dir", "CODEAUDIT_TASKLOG_DIR"); err == nil && v != "" {
			return v
		}
		if dir, err := cfg.Str("runtime.sandbox.interaction_dir", "CODEAUDIT_INTERACTION_DIR"); err == nil && dir != "" {
			return filepath.Join(filepath.Dir(dir), "tasklog")
		}
	}
	return ""
}

// collectUsage — 终态钩子入口（go 异步调用，不拖状态机写锁）。读
// <tasklogDir>/<taskID>/.tasklog/events.jsonl 提取 usage.recorded → PG 全量重算。
// 降级纪律（ADR-237 同型）：任何失败只记日志——计量归集不得反噬任务终态流。
var usageDisabledWarned sync.Once

func (s *TaskServiceImpl) collectUsage(taskID string) {
	if taskID == "" {
		return
	}
	if s.pgStore == nil || tasklogDirForUsage() == "" {
		// 无 PG 档（内存档）或 tasklog 未配置=归集禁用档——部署漂移时零线索
		// 不可接受（对抗式审查 P3），每进程首行 WARN 留痕。
		usageDisabledWarned.Do(func() {
			log.Printf("[task][usage] collect disabled (pgStore=%v tasklogDir=%q) — usage 永不归集，检查部署接线", s.pgStore != nil, tasklogDirForUsage())
		})
		return
	}
	dir := tasklogDirForUsage()
	f, err := os.Open(filepath.Join(dir, taskID, ".tasklog", "events.jsonl"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[task][%s] usage collect: open events.jsonl: %v", taskID, err)
		}
		return // 任务未经 baton 投影（BATON_BIN 禁用/零帧回合）——无输入不归集
	}
	defer f.Close()
	rows := parseUsageEvents(f)
	if err := s.pgStore.replaceUsage(taskID, rows); err != nil {
		log.Printf("[task][%s] usage collect: replace %d rows: %v", taskID, len(rows), err)
		return
	}
	if len(rows) > 0 {
		log.Printf("[task][%s] usage collect ok: %d events", taskID, len(rows))
	}
}

// parseUsageEvents — 逐行解析，仅取 type=usage.recorded；坏行跳过计数（不中断，
// 单行损坏不丢整档归集）。ts 解析失败记零值（不虚构时刻）。
func parseUsageEvents(f *os.File) []usageRow {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 单事件上限 4MiB（blob 引用型载荷防护）
	var rows []usageRow
	bad := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev usageEnvelope
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "usage.recorded" || ev.Usage == nil {
			if err != nil {
				bad++
			}
			continue
		}
		var ts time.Time
		if t, err := time.Parse(time.RFC3339Nano, ev.Ts); err == nil {
			ts = t
		}
		rows = append(rows, usageRow{
			Seq: ev.Seq, Tenant: ev.Tenant, User: ev.User, Session: ev.Session,
			Model: ev.Usage.Model,
			In:    ev.Usage.InputTokens, Out: ev.Usage.OutputTokens,
			CacheR: ev.Usage.CacheReadTokens, CacheW: ev.Usage.CacheWriteTokens,
			Reason: ev.Usage.ReasoningTokens, Total: ev.Usage.TotalTokens,
			RecordedAt: ts,
		})
	}
	if bad > 0 {
		log.Printf("usage parse: %d bad lines skipped", bad)
	}
	// 对抗式审查 P3：Scanner 错误（如单行>4MiB 的 ErrTooLong）会静默停扫——
	// 其后事件全丢且零痕迹；至少留日志（归集降级可见，U8/C4 同源纪律）。
	if err := sc.Err(); err != nil {
		log.Printf("usage parse: scanner stopped early: %v", err)
	}
	return rows
}
