package service

// ADR-225 增量扫描锁定测试：内容 diff 矩阵 / 路径口径 / 基线选定 / 快照复用 /
// 降级矩阵 / 显式基线强契约 / 幂等指纹。设计依据=伞仓 docs/designs/
// incremental-scan-acceptance.md F1-F5/F9；红→绿纪律（ADR-216）适用。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mkTree — 在 dir 下按 file→content 建树（自动建父目录）。
func mkTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

func ts2pb(ts time.Time) *timestamppb.Timestamp { return timestamppb.New(ts) }

// newIncrementalSvc — 直接装配最小 TaskServiceImpl（不触发 NewTaskService 的
// 配置装配路径；仅增量链路所需字段；事件发布器 nil=禁用档 no-op）。
func newIncrementalSvc(t *testing.T, reposDir string) *TaskServiceImpl {
	t.Helper()
	return &TaskServiceImpl{
		tasks:          make(map[string]*pb.ScanTask),
		idem:           make(map[string]*idemRecord),
		stgIdm:         make(map[string]string),
		projectPaths:   make(map[string]string),
		configs:        make(map[string]map[string]string),
		contexts:       make(map[string]*pb.TaskContext),
		logs:           make(map[string][]*pb.TaskLogEntry),
		logIdem:        make(map[string]string),
		incrementalCtx: make(map[string]*orchestrator.IncrementalContext),
		hub:            newTaskWatchHub(),
		reposDir:       reposDir,
	}
}

func mustDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	mkTree(t, dir, files)
	return dir
}

func TestContentDiff_Matrix(t *testing.T) {
	base, err := treeDigest(mustDir(t, map[string]string{
		"app.py":                      "a=1",
		"pkg/util.py":                 "b=2",
		"gone.py":                     "c=3",
		".git/config":                 "x", // 排除目录：不参与 diff
		"same.py":                     "z",
		".codeaudit-incremental.diff": "DUMMY", // 设计排除名：不参与 diff
	}))
	if err != nil {
		t.Fatalf("treeDigest base: %v", err)
	}
	cur, err := treeDigest(mustDir(t, map[string]string{
		"app.py":      "a=2", // 修改
		"pkg/util.py": "b=2", // 未变
		"new.py":      "n=1", // 新增
		"same.py":     "z",
	}))
	if err != nil {
		t.Fatalf("treeDigest cur: %v", err)
	}
	changed, deleted := contentDiff(base, cur)
	if strings.Join(changed, ",") != "app.py,new.py" {
		t.Fatalf("changed=%v（want app.py,new.py 排序确定）", changed)
	}
	if strings.Join(deleted, ",") != "gone.py" {
		t.Fatalf("deleted=%v（want gone.py；.git 与排除文件不得出现）", deleted)
	}
}

func TestTreeDigest_ShellMismatchAlignedByResolveRoot(t *testing.T) {
	// 壳层级不同（一次裸根一次包一层目录）→ 剥壳后 diff 结果与同壳一致（A4.3）
	shell := mustDir(t, map[string]string{"repo-x/app.py": "a=1", "repo-x/lib.py": "l=1"})
	plain := mustDir(t, map[string]string{"app.py": "a=2", "lib.py": "l=1"})
	bd, err1 := treeDigest(ResolveProjectRoot(shell))
	cd, err2 := treeDigest(ResolveProjectRoot(plain))
	if err1 != nil || err2 != nil {
		t.Fatalf("digest: %v %v", err1, err2)
	}
	changed, deleted := contentDiff(bd, cd)
	if len(changed) != 1 || changed[0] != "app.py" || len(deleted) != 0 {
		t.Fatalf("壳错位后 diff 应等价同壳：changed=%v deleted=%v", changed, deleted)
	}
}

func TestNormalizeRepoPath(t *testing.T) {
	cases := map[string]string{
		"./app.py":     "app.py",
		"app.py":       "app.py",
		"pkg//util.py": "pkg/util.py",
		"pkg/./util.py": "pkg/util.py",
		"a/../b.py":    "b.py",
		"./pkg/x.py":   "pkg/x.py",
	}
	for in, want := range cases {
		if got := normalizeRepoPath(in); got != want {
			t.Fatalf("normalizeRepoPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestUnifiedDiff_HunksAndDeletion(t *testing.T) {
	base := mustDir(t, map[string]string{"a.py": "l1\nl2\nl3\nl4\nl5\n", "del.py": "x\n"})
	cur := mustDir(t, map[string]string{"a.py": "l1\nl2-mod\nl3\nl4\nl5\n"})
	diff := unifiedDiffTrees(base, cur, []string{"a.py"}, []string{"del.py"})
	for _, want := range []string{"--- a.py", "+++ a.py", "-l2", "+l2-mod", "@@", "--- del.py", "+++ /dev/null"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("unified diff 缺 %q：\n%s", want, diff)
		}
	}
	// 上下文行在位（diffContextLines=3：改动行 l2 上下文含 l1/l3）
	if !strings.Contains(diff, " l1") || !strings.Contains(diff, " l3") {
		t.Fatalf("diff 缺上下文行：\n%s", diff)
	}
}

func TestUnifiedDiff_BinarySkip(t *testing.T) {
	bin := strings.Repeat("A", 64) + "\x00" + strings.Repeat("B", 64)
	base := mustDir(t, map[string]string{"bin.dat": bin})
	cur := mustDir(t, map[string]string{"bin.dat": strings.Repeat("C", 130)})
	diff := unifiedDiffTrees(base, cur, []string{"bin.dat"}, nil)
	if !strings.Contains(diff, "二进制文件") {
		t.Fatalf("二进制文件应只留说明头：\n%s", diff)
	}
}

func TestDiffHunks_UnchangedReturnsNil(t *testing.T) {
	a := []string{"x", "y", "z"}
	if h := diffHunks(a, a); h != nil {
		t.Fatalf("内容一致应返回 nil（got %v）", h)
	}
}

func TestSelectBaseline_Rules(t *testing.T) {
	dir := t.TempDir()
	s := newIncrementalSvc(t, dir)
	mkCompleted := func(id string, ts time.Time, withTree bool) {
		s.tasks[id] = &pb.ScanTask{TaskId: id, ProjectId: "p1",
			Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(ts)}
		if withTree {
			mkTree(t, filepath.Join(dir, "uploads-"+id, "unpacked"), map[string]string{"a.py": "1"})
		}
	}
	now := time.Now()
	mkCompleted("old", now.Add(-3*time.Hour), true)
	mkCompleted("mid", now.Add(-2*time.Hour), true)
	mkCompleted("newest", now.Add(-1*time.Hour), false) // 无卷树亦无 upload_file_id → 不可达

	got := s.selectBaseline(&pb.ScanTask{TaskId: "cur", ProjectId: "p1"})
	if got.GetTaskId() != "mid" {
		t.Fatalf("基线应回退到最新可达者 mid（got %q）", got.GetTaskId())
	}
	// 跨项目任务不得入选
	s.tasks["other-proj"] = &pb.ScanTask{TaskId: "other-proj", ProjectId: "p2",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(now)}
	mkTree(t, filepath.Join(dir, "uploads-other-proj", "unpacked"), map[string]string{"a.py": "1"})
	if got := s.selectBaseline(&pb.ScanTask{TaskId: "cur", ProjectId: "p1"}); got.GetTaskId() != "mid" {
		t.Fatalf("跨项目任务混入基线候选（got %q）", got.GetTaskId())
	}
	// 非 COMPLETED 任务不得入选
	s.tasks["running"] = &pb.ScanTask{TaskId: "running", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING, CreatedAt: ts2pb(now)}
	mkTree(t, filepath.Join(dir, "uploads-running", "unpacked"), map[string]string{"a.py": "1"})
	if got := s.selectBaseline(&pb.ScanTask{TaskId: "cur", ProjectId: "p1"}); got.GetTaskId() != "mid" {
		t.Fatalf("非终态任务混入基线候选（got %q）", got.GetTaskId())
	}
}

func TestSelectBaseline_NoneReachable(t *testing.T) {
	s := newIncrementalSvc(t, t.TempDir())
	s.tasks["only"] = &pb.ScanTask{TaskId: "only", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now())}
	if got := s.selectBaseline(&pb.ScanTask{TaskId: "cur", ProjectId: "p1"}); got != nil {
		t.Fatalf("无可用基线应返回 nil（got %q）", got.GetTaskId())
	}
}

func TestRunIncrementalDiff_SnapshotAndReuse(t *testing.T) {
	dir := t.TempDir()
	s := newIncrementalSvc(t, dir)
	s.tasks["base-1"] = &pb.ScanTask{TaskId: "base-1", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now())}
	mkTree(t, filepath.Join(dir, "uploads-base-1", "unpacked"), map[string]string{
		"keep.py": "k=1", "mod.py": "m=1", "drop.py": "d=1",
	})
	cur := filepath.Join(dir, "cur")
	mkTree(t, cur, map[string]string{"keep.py": "k=1", "mod.py": "m=2", "add.py": "a=1"})
	task := &pb.ScanTask{TaskId: "inc-1", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		Config: map[string]string{"incremental": "true"}}
	s.tasks["inc-1"] = task
	inc := &orchestrator.IncrementalContext{}
	s.incrementalCtx["inc-1"] = inc

	s.runIncrementalDiff(context.Background(), "inc-1", cur)

	active, baseline, changed, deleted, diffText := inc.Snapshot()
	if !active || baseline != "base-1" {
		t.Fatalf("增量上下文未激活或基线错：active=%v baseline=%q", active, baseline)
	}
	if strings.Join(changed, ",") != "add.py,mod.py" || strings.Join(deleted, ",") != "drop.py" {
		t.Fatalf("changed=%v deleted=%v", changed, deleted)
	}
	if !strings.Contains(diffText, "mod.py") {
		t.Fatalf("diffText 应含变更文件 mod.py")
	}
	if task.GetBaselineTaskId() != "base-1" || task.GetDiffSource() != "content" ||
		len(task.GetChangedFiles()) != 2 || len(task.GetDeletedFiles()) != 1 {
		t.Fatalf("任务快照未回写：baseline=%q diff_source=%q changed=%v deleted=%v",
			task.GetBaselineTaskId(), task.GetDiffSource(), task.GetChangedFiles(), task.GetDeletedFiles())
	}

	// 快照复用（A5.2）：删掉基线卷树后重跑，结果必须不变（重试不重算）
	_ = os.RemoveAll(filepath.Join(dir, "uploads-base-1"))
	s.runIncrementalDiff(context.Background(), "inc-1", cur)
	if task.GetBaselineTaskId() != "base-1" || len(task.GetChangedFiles()) != 2 {
		t.Fatalf("快照复用失效（重算或漂移）：baseline=%q changed=%v",
			task.GetBaselineTaskId(), task.GetChangedFiles())
	}
}

func TestRunIncrementalDiff_DegradeNoBaseline(t *testing.T) {
	s := newIncrementalSvc(t, t.TempDir())
	task := &pb.ScanTask{TaskId: "inc-2", ProjectId: "p2",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		Config: map[string]string{"incremental": "true"}}
	s.tasks["inc-2"] = task
	inc := &orchestrator.IncrementalContext{}
	s.incrementalCtx["inc-2"] = inc

	s.runIncrementalDiff(context.Background(), "inc-2", t.TempDir())

	if active, _, _, _, _ := inc.Snapshot(); active {
		t.Fatalf("无基线时不得激活增量")
	}
	if reason := task.GetConfig()["incremental_degraded_reason"]; reason != "no_baseline" {
		t.Fatalf("降级原因=%q（want no_baseline）", reason)
	}
	if task.GetBaselineTaskId() != "" || task.GetDiffSource() != "" {
		t.Fatalf("降级任务不得携带增量快照")
	}
}

func TestCreateScanTask_ExplicitBaselineContract(t *testing.T) {
	cases := []struct {
		name     string
		baseline *pb.ScanTask
		wantErr  string
	}{
		{"not found", nil, "not found"},
		{"cross project", &pb.ScanTask{TaskId: "base-y", ProjectId: "other",
			Status: pb.TaskStatus_TASK_STATUS_COMPLETED}, "belongs to project"},
		{"not completed", &pb.ScanTask{TaskId: "base-z", ProjectId: "p1",
			Status: pb.TaskStatus_TASK_STATUS_RUNNING}, "not COMPLETED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newIncrementalSvc(t, t.TempDir())
			if c.baseline != nil {
				s.tasks[c.baseline.GetTaskId()] = c.baseline
			}
			_, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
				Metadata:       &pb.RequestMetadata{RequestId: "inc-new"},
				ProjectId:      "p1",
				ScanMode:       pb.ScanMode_SCAN_MODE_SAST_ONLY,
				Incremental:    true,
				BaselineTaskId: map[string]string{
					"not found":     "base-x",
					"cross project": "base-y",
					"not completed": "base-z",
				}[c.name],
			})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("显式基线 %s 应创建即 4xx（got %v）", c.name, err)
			}
		})
	}
	// 合法显式基线：创建成功且快照落任务
	s := newIncrementalSvc(t, t.TempDir())
	s.tasks["base-ok"] = &pb.ScanTask{TaskId: "base-ok", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now())}
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:       &pb.RequestMetadata{RequestId: "inc-ok"},
		ProjectId:      "p1",
		ScanMode:       pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Incremental:    true,
		BaselineTaskId: "base-ok",
		GitAnchor:      &pb.GitAnchor{Commit: "abc123", Branch: "main", Dirty: true},
		DiffHint:       "M\tsome.py\n",
	})
	if err != nil {
		t.Fatalf("合法显式基线创建失败: %v", err)
	}
	if task.GetBaselineTaskId() != "base-ok" || task.GetGitAnchor().GetCommit() != "abc123" ||
		task.GetConfig()["incremental"] != "true" || task.GetConfig()["diff_hint"] == "" {
		t.Fatalf("增量字段未落任务：baseline=%q anchor=%v config=%v",
			task.GetBaselineTaskId(), task.GetGitAnchor(), task.GetConfig())
	}
}

func TestFingerprint_IncrementalFieldsDiffer(t *testing.T) {
	a := &pb.CreateScanTaskRequest{ProjectId: "p", Config: map[string]string{"k": "v"}}
	b := &pb.CreateScanTaskRequest{ProjectId: "p", Config: map[string]string{"k": "v"}, Incremental: true}
	if fingerprintCreate(a) == fingerprintCreate(b) {
		t.Fatalf("增量意图必须进入幂等指纹（同键异体判定失明）")
	}
	c := &pb.CreateScanTaskRequest{ProjectId: "p", Config: map[string]string{"k": "v"},
		Incremental: true, GitAnchor: &pb.GitAnchor{Commit: "abc"}}
	d := &pb.CreateScanTaskRequest{ProjectId: "p", Config: map[string]string{"k": "v"},
		Incremental: true, GitAnchor: &pb.GitAnchor{Commit: "def"}}
	if fingerprintCreate(c) == fingerprintCreate(d) {
		t.Fatalf("git 锚点 commit 必须进入幂等指纹")
	}
}

// A6.3（验收 F6 补齐；R59 2026-09-11 修正）: 增量任务执行日志必须含一行视野声明——
// 经生产调用链（runIncrementalDiff 激活点）触发，不得直调 helper（R58/A6.3 两轮教训：
// 直调用例绕开生产时序，曾令声明在真实任务上永不可达而测试全绿）。
func TestIncrementalScopeNotice_ViaProductionChain(t *testing.T) {
	dir := t.TempDir()
	s := newIncrementalSvc(t, dir)
	s.tasks["base-scope"] = &pb.ScanTask{TaskId: "base-scope", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now())}
	mkTree(t, filepath.Join(dir, "uploads-base-scope", "unpacked"), map[string]string{
		"keep.py": "k=1", "mod.py": "m=1",
	})
	cur := filepath.Join(dir, "cur")
	mkTree(t, cur, map[string]string{"keep.py": "k=1", "mod.py": "m=2"})
	s.tasks["inc-scope"] = &pb.ScanTask{TaskId: "inc-scope", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		Config: map[string]string{"incremental": "true"}}
	inc := &orchestrator.IncrementalContext{}
	s.incrementalCtx["inc-scope"] = inc

	s.runIncrementalDiff(context.Background(), "inc-scope", cur) // 生产链入口

	resp, err := s.GetTaskLogs(context.Background(), &pb.GetTaskLogsRequest{TaskId: "inc-scope"})
	if err != nil {
		t.Fatalf("GetTaskLogs: %v", err)
	}
	found := false
	for _, e := range resp.GetLogs() {
		if strings.Contains(e.GetMessage(), "增量扫描以文件为边界") && strings.Contains(e.GetMessage(), "全量精度请选全量") {
			found = true
		}
	}
	if !found {
		t.Fatalf("scope notice missing via production chain (A6.3/R59)")
	}
	// 零变更（同树）不触发视野声明（零扫全继承，无扫描即无声明）
	mkTree(t, filepath.Join(dir, "cur2"), map[string]string{"keep.py": "k=1", "mod.py": "m=1"})
	s.tasks["inc-zero"] = &pb.ScanTask{TaskId: "inc-zero", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING,
		Config: map[string]string{"incremental": "true"}}
	s.incrementalCtx["inc-zero"] = &orchestrator.IncrementalContext{}
	s.runIncrementalDiff(context.Background(), "inc-zero", filepath.Join(dir, "cur2"))
	resp2, _ := s.GetTaskLogs(context.Background(), &pb.GetTaskLogsRequest{TaskId: "inc-zero"})
	for _, e := range resp2.GetLogs() {
		if strings.Contains(e.GetMessage(), "增量扫描以文件为边界") {
			t.Fatalf("zero-change task must not emit scope notice, got: %s", e.GetMessage())
		}
	}
}
