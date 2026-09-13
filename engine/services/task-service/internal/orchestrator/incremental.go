// incremental.go — 增量扫描编排传导（ADR-225）。
//
// 设计依据: 伞仓 docs/designs/incremental-scan.md §3/§4.5/§4.6/§4.7——
// task-service 在 Prepare 阶段完成基线选定与内容 diff 后，经本上下文向编排各阶段传导：
//   - Execute 前置: result-service InheritFindings（未变更文件 findings 物化继承）
//   - runMultipleScans: changed_files → sast-adapter（仅扫变更文件）
//   - runAIAnalysis: 变更清单 + incremental_diff → dsh-runtime（增量聚焦提示词）
// 降级口径: task-service 侧已把"无基线/diff 失败"降级为全量（inc 不激活），编排零感知。
package orchestrator

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/codeaudit/proto-gen"
)

// IncrementalContext — Prepare（生产方=task-service）与编排阶段（消费方）之间的
// 线程安全增量上下文。Prepare 在编排协程内执行，写方单线程；阶段消费并发读。
type IncrementalContext struct {
	mu       sync.Mutex
	active   bool
	baseline string
	changed  []string
	deleted  []string
	diffText string
}

// Set — 生产方填充（task-service Prepare 内，diff 快照回写任务后调用）。
func (c *IncrementalContext) Set(active bool, baseline string, changed, deleted []string, diffText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = active
	c.baseline = baseline
	c.changed = append([]string(nil), changed...)
	c.deleted = append([]string(nil), deleted...)
	c.diffText = diffText
}

// Snapshot — 消费方取只读拷贝。
func (c *IncrementalContext) Snapshot() (active bool, baseline string, changed, deleted []string, diffText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active, c.baseline,
		append([]string(nil), c.changed...), append([]string(nil), c.deleted...), c.diffText
}

// runIncrementalInherit — 模式分支前置：未变更文件 findings 继承（ADR-225 §4.6）。
// 幂等键用任务稳定键（不带尝试序号）：自动重试重放时 result 侧按
// (request_id, finding_id) 命中已继承行直接跳过，不产生重复继承。
// 继承失败=编排失败走既有重试链：完整性优先（缺继承行的"完整视图"是假完整，
// 诚实失败比重跑出一个不完整的成功更符合降级纪律）。
func (o *Orchestrator) runIncrementalInherit(ctx context.Context, r RunRequest, stage StageRecorder) error {
	active, baseline, changed, deleted, _ := r.Incremental.Snapshot()
	if !active {
		return nil
	}
	exclude := append(append([]string(nil), changed...), deleted...)
	conn, closeFn, err := dial(o.cfg.ResultAddr)
	if err != nil {
		return fmt.Errorf("dial result-service for inherit: %w", err)
	}
	defer closeFn()
	client := pb.NewResultServiceClient(conn)
	iCtx, iCancel := ctx, context.CancelFunc(func() {})
	defer iCancel()
	resp, err := client.InheritFindings(iCtx, &pb.InheritFindingsRequest{
		Metadata:       md(r.TaskID + "-inherit"),
		BaselineTaskId: baseline,
		NewTaskId:      r.TaskID,
		ExcludePaths:   exclude,
	})
	if err != nil {
		return fmt.Errorf("InheritFindings: %w", err)
	}
	// R60部分失败必须失败——FailedCount 被吞没时缺继承行的任务
	// 照常 COMPLETED，"完整视图"是假完整；走既有 FAILED→QUEUED 重试链（幂等重放
	// 跳过已继承行，重试只补缺）。
	if n := resp.GetFailedCount(); n > 0 {
		return fmt.Errorf("InheritFindings 部分失败：继承 %d 条中 %d 条落库失败（FailedCount=%d）",
			resp.GetInheritedCount()+n, n, n)
	}
	stage("incremental", fmt.Sprintf("基线 %s：继承 %d 条（跳过变更/删除文件 %d 条）",
		baseline, resp.GetInheritedCount(), resp.GetSkippedCount()))
	return nil
}
