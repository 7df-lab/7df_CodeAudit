package service

// D2 S2 换手入口：PauseTask 携 agent=pause 即换手点。
// 转换成立后换笔（transition 失败不改快照）；resume 生效链=编排每次 Execute 前
// RunRequest.Agent 动态读取（既有 L651 链）+runtime SelectAgent 每回合执行。

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
)

func runningTask(t *testing.T, s *TaskServiceImpl, id string) {
	t.Helper()
	task := createTask(t, s, id, pb.ScanMode_SCAN_MODE_AI_ONLY)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tasks[task.GetTaskId()]
	if !ok {
		t.Fatal("任务未入映射表")
	}
	if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); terr != nil {
		t.Fatalf("transition RUNNING: %v", terr)
	}
}

func TestPauseTask_HandoffAgent(t *testing.T) {
	s := newSvc(t)
	runningTask(t, s, "req-ho-1")
	tid := "gw-ho-1"
	for k := range s.tasks {
		tid = k
	} // createTask 生成的真实 id

	cur, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid, Agent: "maka-acp"})
	if err != nil {
		t.Fatalf("pause+handoff: %v", err)
	}
	if cur.GetStatus() != pb.TaskStatus_TASK_STATUS_PAUSED {
		t.Fatalf("应 PAUSED: %v", cur.GetStatus())
	}
	if cur.GetAgent() != "maka-acp" {
		t.Fatalf("换笔未生效: agent=%q（S2 换手入口回归）", cur.GetAgent())
	}
	// resume 后编排动态读取链的源=任务快照（RunRequest.Agent=task.GetAgent()）——
	// 快照已换即新 agent 生效（驻留补推 R129 唤醒后同链）
	if got := s.tasks[tid].GetAgent(); got != "maka-acp" {
		t.Fatalf("内存活实体快照未换: %q", got)
	}
}

func TestPauseTask_HandoffAgentInvalid(t *testing.T) {
	s := newSvc(t)
	runningTask(t, s, "req-ho-2")
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	if _, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid, Agent: "Maka!"}); err == nil {
		t.Fatal("非法 agent 应拒绝")
	}
	// 原子性（校验先于 transition）：非法值整请求拒绝——状态不动+快照不污染
	if got := s.tasks[tid].GetStatus(); got != pb.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("非法 agent 拒绝路径不得迁移状态: %v", got)
	}
	if got := s.tasks[tid].GetAgent(); got == "Maka!" {
		t.Fatal("非法 agent 污染快照")
	}
}

func TestPauseTask_NoAgentKeepsSnapshot(t *testing.T) {
	s := newSvc(t)
	runningTask(t, s, "req-ho-3")
	tid := ""
	for k := range s.tasks {
		tid = k
	}
	before := s.tasks[tid].GetAgent()
	if _, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid}); err != nil {
		t.Fatal(err)
	}
	if got := s.tasks[tid].GetAgent(); got != before {
		t.Fatalf("无 agent 的 pause 不应改快照: %q -> %q", before, got)
	}
}

// handoffOrch — 记录每次 Execute 收到的 agent（换笔重入断言：第二次执行必须
// 拿到新 agent——驻留唤醒后的旧协程续跑则拿到旧值）。首回合阻塞到 release
// 关闭（模拟真实回合窗口——pause 落点必须在回合进行中才有状态机落点）。
type handoffOrch struct {
	mu      sync.Mutex
	agents  []string
	release chan struct{}
}

func (f *handoffOrch) record(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = append(f.agents, a)
}

func (f *handoffOrch) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.agents...)
}

func (f *handoffOrch) Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error) {
	f.record(r.Agent)
	if len(f.snapshot()) == 1 {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]interface{}{"graph_nodes_total": 1}, nil
}

func TestPauseTask_HandoffRetryReentersWithNewAgent(t *testing.T) {
	s := newSvc(t)
	task := createTask(t, s, "req-ho-4", pb.ScanMode_SCAN_MODE_AI_ONLY)
	tid := task.GetTaskId()
	s.mu.Lock()
	cur0 := s.tasks[tid]
	if terr := s.transitionLocked(cur0, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); terr != nil {
		s.mu.Unlock()
		t.Fatalf("transition: %v", terr)
	}
	s.mu.Unlock()

	fake := &handoffOrch{release: make(chan struct{})}
	s.orch = fake
	entry := &cancelEntry{cancel: func() {}}
	r := orchestrator.RunRequest{TaskID: tid, RequestID: "req-ho-4", Agent: "dsh"}
	done := make(chan struct{})
	go func() {
		s.runOrchestration(context.Background(), entry, fake, r, func(string, string) {})
		close(done)
	}()

	// 首回合进行中（fake 阻塞）落 pause+换笔——状态机落点=RUNNING→PAUSED
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(fake.snapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.PauseTask(context.Background(), &pb.PauseTaskRequest{TaskId: tid, Agent: "maka-acp"}); err != nil {
		t.Fatalf("pause+handoff: %v", err)
	}
	close(fake.release) // 首回合收束（gate 语义=当前回合跑完）
	// 等待驻留注册（编排协程在 Execute 返回后见 PAUSED 驻留）
	for time.Now().Before(deadline) {
		s.mu.RLock()
		_, waiting := s.resumes[tid]
		s.mu.RUnlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.ResumeTask(context.Background(), &pb.ResumeTaskRequest{TaskId: tid}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("换笔 resume 后编排未收束")
	}
	got := fake.snapshot()
	if len(got) != 2 {
		t.Fatalf("换笔应重入 Execute（2 次）: %d 次 agents=%v", len(got), got)
	}
	if got[0] != "dsh" || got[1] != "maka-acp" {
		t.Fatalf("重入未拿到新 agent（换手链断）: %v", got)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.tasks[tid].GetStatus(); st != pb.TaskStatus_TASK_STATUS_COMPLETED {
		t.Fatalf("换笔重入后应 COMPLETED: %v", st)
	}
}

// M106 牙齿：换笔重入摘除——resume 后旧协程旧 agent 续跑（换手链断在重入环）。
func TestPauseTask_HandoffRetryBlockRemoval(t *testing.T) {
	// 复用 HandoffRetryReentersWithNewAgent 场景（M106 变异的锁定测试同款）——
	// 该测试断言第二次 Execute 拿到新 agent+COMPLETED，重入摘除即红。
	t.Run("同款场景锚", func(t *testing.T) { TestPauseTask_HandoffRetryReentersWithNewAgent(t) })
}
