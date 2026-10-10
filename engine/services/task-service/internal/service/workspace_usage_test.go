package service

// ADR-247（切片3后半）锁定测试：
//   ① workspace=team 贯穿：taskOriginOf 派生（config 键）+ 创建时快照接线不变量；
//   ② usage 计量归集：events.jsonl 解析（多形态容错）+ 计费口径聚合 + 内存档诚实降级
//      + tasklog 目录部署覆盖口（R36 同型回归面）。
// 端到端（sim 一单后按 task 查 usage / 真 PG 幂等重算）归 pb-A sim 门禁。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// TestTaskOrigin_WorkspaceFromConfig — config["workspace"]（创建时快照）→
// TaskOrigin.workspace 派生；无键=空串（回落链语义，不虚构）。
func TestTaskOrigin_WorkspaceFromConfig(t *testing.T) {
	with := taskOriginOf(&pb.ScanTask{
		CreatedBy: "u1",
		Config:    map[string]string{"workspace": "team-a"},
	})
	if with.GetWorkspace() != "team-a" || with.GetUser() != "u1" {
		t.Fatalf("origin = %+v", with)
	}
	without := taskOriginOf(&pb.ScanTask{CreatedBy: "u1"})
	if without.GetWorkspace() != "" {
		t.Fatalf("no workspace key must stay empty (fallback chain), got %q", without.GetWorkspace())
	}
	if taskOriginOf(nil) != nil {
		t.Fatal("nil task → nil origin")
	}
}

// TestCreateScanTask_WorkspaceSnapshotWired — 结构性守卫（R77/pg_bounded 同型）：
// 创建时快照接线=锁外预查 + team 非空写约定键。拆任一环节（锁内查/不写键）即红。
func TestCreateScanTask_WorkspaceSnapshotWired(t *testing.T) {
	src, err := os.ReadFile("task_service.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	createSeg := body[strings.Index(body, "func (s *TaskServiceImpl) CreateScanTask"):strings.Index(body, "func (s *TaskServiceImpl) GetScanTask")]
	if createSeg == "" {
		t.Fatal("CreateScanTask segment not found")
	}
	// 锁外预查：fetchProjectTeam 调用必须先于 s.mu.Lock()
	lockAt := strings.Index(createSeg, "s.mu.Lock()")
	fetchAt := strings.Index(createSeg, "s.fetchProjectTeam(")
	if lockAt < 0 || fetchAt < 0 || fetchAt > lockAt {
		t.Fatal("ADR-247: fetchProjectTeam 必须在 CreateScanTask 持锁前调用（锁内禁网络调用）")
	}
	if !strings.Contains(createSeg, `task.Config["workspace"] = team`) {
		t.Fatal("ADR-247: team 非空必须写 config[\"workspace\"] 约定键（创建时快照）")
	}
}

// TestTransition_TerminalHookWired — 终态 usage 归集钩子接线不变量（行为面：
// 异步 collectUsage 幂等全量重算，真 PG 验证归 sim）。
func TestTransition_TerminalHookWired(t *testing.T) {
	src, err := os.ReadFile("task_service.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	seg := body[strings.Index(body, "func (s *TaskServiceImpl) transitionLocked"):]
	seg = seg[:strings.Index(seg, "\n}")]
	if !strings.Contains(seg, "IsTerminal") || !strings.Contains(seg, "collectUsage") {
		t.Fatal("ADR-247: transitionLocked 终态必须异步触发 collectUsage（公理 D2）")
	}
}

// TestParseUsageEvents — baton events.jsonl 解析：usage.recorded 提取（envelope
// 打标+payload 全字段）、非 usage 事件跳过、坏行跳过不中断、ts 解析。
func TestParseUsageEvents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	fixture := strings.Join([]string{
		`{"seq":1,"ts":"2026-09-18T10:00:00Z","task":"T-1","tenant":"acme","user":"u1","session":"main","type":"task.created","goal":"x"}`,
		`{"seq":2,"ts":"2026-09-18T10:00:01Z","task":"T-1","tenant":"acme","user":"u1","session":"main","pen":{"agent":"dsh","model":"deepseek-chat-v3.2"},"type":"usage.recorded","usage":{"model":"deepseek-chat-v3.2","input_tokens":1200,"output_tokens":80,"cache_read_tokens":300,"cache_write_tokens":100,"reasoning_tokens":50,"total_tokens":1680}}`,
		`{not-json`,
		`{"seq":3,"ts":"2026-09-18T10:00:02Z","task":"T-1","type":"tool.call","call_id":"c1"}`,
		`{"seq":4,"ts":"2026-09-18T10:00:03Z","task":"T-1","tenant":"acme","session":"verify","pen":{"agent":"dsh"},"type":"usage.recorded","usage":{"model":"glm-5.3","input_tokens":10,"output_tokens":5}}`,
		``,
	}, "\n")
	if err := os.WriteFile(p, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := parseUsageEvents(f)
	if len(rows) != 2 {
		t.Fatalf("usage rows: want 2, got %d", len(rows))
	}
	r := rows[0]
	if r.Seq != 2 || r.Tenant != "acme" || r.User != "u1" || r.Session != "main" || r.Model != "deepseek-chat-v3.2" {
		t.Fatalf("row0 = %+v", r)
	}
	if r.In != 1200 || r.Out != 80 || r.CacheR != 300 || r.CacheW != 100 || r.Reason != 50 || r.Total != 1680 {
		t.Fatalf("row0 tokens = %+v", r)
	}
	if r.RecordedAt.IsZero() || r.RecordedAt.Year() != 2026 {
		t.Fatalf("row0 ts = %v", r.RecordedAt)
	}
	if rows[1].Session != "verify" || rows[1].User != "" { // user 缺失=空串（不虚构）
		t.Fatalf("row1 = %+v", rows[1])
	}
}

// TestUsageAcc_BillingBasis — 计费口径：total = input+cache_read+cache_write+output
// （reasoning 通常已含于 output，不重复计入——proto TaskModelUsage 注释即契约）。
func TestUsageAcc_BillingBasis(t *testing.T) {
	m := &pb.TaskModelUsage{}
	acc(m, usageRow{In: 100, Out: 40, CacheR: 10, CacheW: 5, Reason: 30})
	acc(m, usageRow{In: 1, Out: 1})
	if m.Calls != 2 {
		t.Fatalf("calls = %d", m.Calls)
	}
	if want := int64(100 + 40 + 10 + 5 + 1 + 1); m.TotalTokens != want {
		t.Fatalf("total = %d, want %d（reasoning 30 不重复计入）", m.TotalTokens, want)
	}
	if m.InputTokens != 101 || m.OutputTokens != 41 || m.CacheReadTokens != 10 || m.CacheWriteTokens != 5 {
		t.Fatalf("buckets = %+v", m)
	}
}

// TestGetTaskUsage_MemoryTier — 内存档（无 PG）：任务存在=空聚合（诚实，非报错）；
// 任务不存在=NotFound；task_id 缺失=InvalidArgument。
func TestGetTaskUsage_MemoryTier(t *testing.T) {
	s := NewTaskService()
	s.tasks["t-1"] = &pb.ScanTask{TaskId: "t-1"}

	if _, err := s.GetTaskUsage(nil, &pb.GetTaskUsageRequest{TaskId: ""}); err == nil {
		t.Fatal("empty task_id must be InvalidArgument")
	}
	if _, err := s.GetTaskUsage(nil, &pb.GetTaskUsageRequest{TaskId: "nope"}); err == nil {
		t.Fatal("unknown task must be NotFound")
	}
	sum, err := s.GetTaskUsage(nil, &pb.GetTaskUsageRequest{TaskId: "t-1"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.GetTaskId() != "t-1" || sum.GetTotals() == nil || len(sum.GetModels()) != 0 {
		t.Fatalf("memory-tier summary = %+v（空聚合即真实状态）", sum)
	}
}

// TestTasklogDirForUsage_EnvOverride — 部署覆盖口（R36 同型回归面）：
// CODEAUDIT_TASKLOG_DIR 显式配置优先；缺省派生 <interaction_dir>/../tasklog
// 与 runtime baton_hook.tasklogDir 同源（两服务必须同指共享卷）。
func TestTasklogDirForUsage_EnvOverride(t *testing.T) {
	t.Setenv("CODEAUDIT_TASKLOG_DIR", "/data/repos/tasklog")
	if got := tasklogDirForUsage(); got != "/data/repos/tasklog" {
		t.Fatalf("env override must win: got %q", got)
	}
}

// TestCreateScanTask_StripsRequestWorkspaceKey — ADR-247 对抗式审查 P1 锁定：
// workspace 键合法写入者只有 team 快照。请求 config 自带 workspace（测试环境无
// project-service=预查失败 team 空）不得存活——否则跨 team 命名空间寻址（沙箱
// 归属+推理路由配额面）。
func TestCreateScanTask_StripsRequestWorkspaceKey(t *testing.T) {
	s := newSvc(t)
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-ws-strip"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_AI_ONLY,
		Config:    map[string]string{"workspace": "evil-team", "other": "kept"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v, ok := task.GetConfig()["workspace"]; ok {
		t.Fatalf("request-injected workspace must be stripped, got %q (cross-team addressing)", v)
	}
	if task.GetConfig()["other"] != "kept" { // 剥离只针对治理性内部键
		t.Fatalf("unrelated config key lost: %v", task.GetConfig())
	}
	if taskOriginOf(task).GetWorkspace() != "" {
		t.Fatalf("origin must stay empty when team unresolved, got %q", taskOriginOf(task).GetWorkspace())
	}
}

// TestCreateScanTask_StripsGovernedKeys — 治理性内部键清单（ADR-247 P1+ADR-248
// 复审 P1 同型复发实录；ADR-249 扩集 execution）：凡服务端快照的治理键都必须在此
// 剥——新加键漏剥即本测试红驱动补丁（结构性防第三型复发）。
func TestCreateScanTask_StripsGovernedKeys(t *testing.T) {
	s := newSvc(t)
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "req-gov-strip"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"network_profile": "targeted", "execution": "graph"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v, ok := task.GetConfig()["network_profile"]; ok {
		t.Fatalf("request-injected network_profile must be stripped (pack 档位治理绕过), got %q", v)
	}
	if o := taskOriginOf(task); o.GetNetworkProfile() != "" {
		t.Fatalf("origin profile must stay empty without pack, got %q", o.GetNetworkProfile())
	}
	if v, ok := task.GetConfig()["execution"]; ok {
		t.Fatalf("request-injected execution must be stripped (pack 执行形态治理绕过，ADR-249), got %q", v)
	}
}

// TestUsageInsertChunk_Shape — 二轮复审 P3-2 锁定：占位编号块内连续无重叠
// （动词数=参数数-1，编号恰覆盖 2..13×N+1），列序与 usageRow 字段序一致。
func TestUsageInsertChunk_Shape(t *testing.T) {
	rows := make([]usageRow, 3)
	for i := range rows {
		rows[i] = usageRow{Seq: int64(i + 1), Model: "m", In: int64(i), Out: int64(i * 2)}
	}
	q, args := usageInsertChunk("t-1", rows)
	if len(args) != 1+3*12 { // $1（task_id 行级）+每行 12 值（seq..recorded_at）
		t.Fatalf("args = %d, want %d", len(args), 1+3*12)
	}
	// 编号覆盖：$1（task_id，每行字面重复引用=合法 PG）+ 行值编号 2..1+N*12
	// 恰出现；任何越界编号（>len(args)）或 >1 编号缺失=列序/编号断链即炸。
	nums := map[int]int{} // 编号 → 引用次数
	for _, tok := range strings.Split(q, "$") {
		d := 0
		for _, c := range tok {
			if c < '0' || c > '9' {
				break
			}
			d = d*10 + int(c-'0')
		}
		if d > 0 {
			nums[d]++
		}
	}
	if nums[1] != len(rows) { // $1 每行引用一次
		t.Fatalf("$1 referenced %d times, want %d (rows)", nums[1], len(rows))
	}
	for n := 2; n <= len(args); n++ {
		if nums[n] != 1 {
			t.Fatalf("placeholder $%d referenced %d times (want 1) — 编号断链/重复", n, nums[n])
		}
	}
	if len(nums) != len(args) {
		t.Fatalf("distinct placeholders %d != args %d (越界编号)", len(nums), len(args))
	}
}
