package service

// ADR-225 S5 卷缓存生命周期锁定测试（验收 F12/F13）：三触发线 + 硬保护。
// 判杀面=验收全局底线③"误删零容忍"（运行中/memory 档/邻居目录）。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
	"google.golang.org/grpc"
)

// fakeStorageServer — 最小 StorageService：档位与锚点 HEAD 可注入。
type fakeStorageServer struct {
	pb.UnimplementedStorageServiceServer
	mode    string
	fileOK  map[string]bool
	objects map[string]dlObject
}

func (f *fakeStorageServer) GetStorageMode(ctx context.Context, _ *pb.GetStorageModeRequest) (*pb.GetStorageModeResponse, error) {
	return &pb.GetStorageModeResponse{Mode: f.mode}, nil
}

func (f *fakeStorageServer) GetFileInfo(ctx context.Context, req *pb.GetFileInfoRequest) (*pb.StoredFile, error) {
	if f.fileOK[req.GetFileId()] {
		return &pb.StoredFile{FileId: req.GetFileId(), FilePath: "trees/x.tar.gz"}, nil
	}
	if obj, ok := f.objects[req.GetFileId()]; ok {
		return &pb.StoredFile{FileId: req.GetFileId(), FilePath: obj.name}, nil
	}
	return nil, os.ErrNotExist
}

func startFakeStorage(t *testing.T, f *fakeStorageServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterStorageServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func newGC(t *testing.T, reposDir string) (*TaskServiceImpl, *repoCacheGC) {
	t.Helper()
	s := newIncrementalSvc(t, reposDir)
	g := &repoCacheGC{s: s, enabled: true, interval: time.Hour,
		ttl: 24 * time.Hour, orphanTTL: 7 * 24 * time.Hour, maxBytes: 1 << 40}
	return s, g
}

func mkTaskTree(t *testing.T, s *TaskServiceImpl, id string, status pb.TaskStatus, age time.Duration, tarID string) string {
	t.Helper()
	dir := filepath.Join(s.reposDir, "uploads-"+id, "unpacked")
	mkTree(t, dir, map[string]string{"a.py": "1"})
	old := time.Now().Add(-age)
	_ = os.Chtimes(dir, old, old)
	cfg := map[string]string{}
	if tarID != "" {
		cfg["tree_tar_file_id"] = tarID
	}
	s.tasks[id] = &pb.ScanTask{TaskId: id, ProjectId: "p1", Status: status,
		CreatedAt: ts2pb(old), UpdatedAt: ts2pb(old), Config: cfg}
	return dir
}

func TestRepoCacheGC_HardProtections(t *testing.T) {
	// memory 档：全部触发线停用（A13.6——对象在内存重启即失，删=丢数据）
	t.Run("memory 档绝不删", func(t *testing.T) {
		addr := startFakeStorage(t, &fakeStorageServer{mode: "memory", fileOK: map[string]bool{"tar-1": true}})
		t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
		s, g := newGC(t, t.TempDir())
		dir := mkTaskTree(t, s, "t-old", pb.TaskStatus_TASK_STATUS_COMPLETED, 72*time.Hour, "tar-1")
		g.tick()
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("memory 档删除了卷树（A13.6 红线）: %v", err)
		}
	})
	// 运行中/PAUSED/FAILED（自动重试在途）不删（A13.2 + FAILED 排除）
	t.Run("非真终态绝不删", func(t *testing.T) {
		addr := startFakeStorage(t, &fakeStorageServer{mode: "s3", fileOK: map[string]bool{"tar-1": true}})
		t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
		s, g := newGC(t, t.TempDir())
		for _, c := range []struct {
			id     string
			status pb.TaskStatus
		}{
			{"t-run", pb.TaskStatus_TASK_STATUS_RUNNING},
			{"t-pause", pb.TaskStatus_TASK_STATUS_PAUSED},
			{"t-failed", pb.TaskStatus_TASK_STATUS_FAILED},
		} {
			mkTaskTree(t, s, c.id, c.status, 72*time.Hour, "tar-1")
		}
		g.tick()
		for _, id := range []string{"t-run", "t-pause", "t-failed"} {
			if _, err := os.Stat(filepath.Join(s.reposDir, "uploads-"+id)); err != nil {
				t.Fatalf("非真终态任务 %s 的树被删（红线）: %v", id, err)
			}
		}
	})
	// 邻居目录绝不触碰（ai-interaction=R36；.gateway-cache=gateway 缓存）
	t.Run("邻居目录保护", func(t *testing.T) {
		addr := startFakeStorage(t, &fakeStorageServer{mode: "s3", fileOK: map[string]bool{}})
		t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
		s, g := newGC(t, t.TempDir())
		for _, name := range []string{"ai-interaction", ".gateway-cache", ".gc-stale"} {
			mkTree(t, filepath.Join(s.reposDir, name), map[string]string{"x": "1"})
			old := time.Now().Add(-8 * 24 * time.Hour)
			_ = os.Chtimes(filepath.Join(s.reposDir, name), old, old)
		}
		g.tick()
		for _, name := range []string{"ai-interaction", ".gateway-cache", ".gc-stale"} {
			if _, err := os.Stat(filepath.Join(s.reposDir, name)); err != nil {
				t.Fatalf("保护目录 %s 被删（红线）: %v", name, err)
			}
		}
	})
}

func TestRepoCacheGC_TTLLine(t *testing.T) {
	addr := startFakeStorage(t, &fakeStorageServer{mode: "s3", fileOK: map[string]bool{"tar-ok": true}})
	t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
	s, g := newGC(t, t.TempDir())
	// 终态 + 超 TTL + 锚点在桶 → 删（改名隔离后清空）
	old := mkTaskTree(t, s, "t-evict", pb.TaskStatus_TASK_STATUS_COMPLETED, 72*time.Hour, "tar-ok")
	// 无锚点（入桶失败的老任务）→ 保留（诚实降级：宁可占盘不可丢唯一副本）
	keep1 := mkTaskTree(t, s, "t-keep1", pb.TaskStatus_TASK_STATUS_COMPLETED, 72*time.Hour, "")
	// 锚点不在桶（HEAD 失败）→ 保留
	keep2 := mkTaskTree(t, s, "t-keep2", pb.TaskStatus_TASK_STATUS_COMPLETED, 72*time.Hour, "tar-missing")
	// 窗口内 → 保留
	keep3 := mkTaskTree(t, s, "t-keep3", pb.TaskStatus_TASK_STATUS_COMPLETED, time.Hour, "tar-ok")
	g.tick()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("TTL 线未驱逐: %v", err)
	}
	for _, d := range []string{keep1, keep2, keep3} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("不应删除的卷树被删（%s）: %v", d, err)
		}
	}
}

func TestRepoCacheGC_OrphanLine(t *testing.T) {
	addr := startFakeStorage(t, &fakeStorageServer{mode: "s3", fileOK: map[string]bool{}})
	t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
	s, g := newGC(t, t.TempDir())
	orphan := filepath.Join(s.reposDir, "uploads-ghost", "unpacked")
	mkTree(t, orphan, map[string]string{"a": "1"})
	old := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(s.reposDir, "uploads-ghost"), old, old)
	freshOrphan := filepath.Join(s.reposDir, "uploads-fresh", "unpacked")
	mkTree(t, freshOrphan, map[string]string{"a": "1"})
	g.tick()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿目录超 TTL 未清理: %v", err)
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Fatalf("孤儿窗口内的目录被误删: %v", err)
	}
}

func TestWriteTreeTarGz_RoundtripAndExcludes(t *testing.T) {
	root := mustDir(t, map[string]string{
		"app.py": "x=1", "pkg/u.py": "y=2",
		".git/config":                 "g", // 排除
		".codeaudit-incremental.diff": "D", // 排除（与内容 diff 同口径）
	})
	tmp := filepath.Join(t.TempDir(), "tree.tar.gz")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := writeTreeTarGz(f, root)
	f.Close()
	if err != nil || n != 2 {
		t.Fatalf("writeTreeTarGz: n=%d err=%v（want 2 文件，排除项不得入包）", n, err)
	}
	rf, _ := os.Open(tmp)
	defer rf.Close()
	gz, gzErr := gzip.NewReader(rf)
	if gzErr != nil {
		t.Fatalf("gzip: %v", gzErr)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			t.Fatalf("read tar: %v", herr)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "app.py,pkg/u.py" {
		t.Fatalf("tar 内容=%v（排除失效）", names)
	}
}

func TestQuarantineRemove(t *testing.T) {
	dir := mustDir(t, map[string]string{"a.py": "1"})
	if err := quarantineRemove(dir); err != nil {
		t.Fatalf("quarantineRemove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("目录未删除")
	}
	// 不存在目录：幂等报错（调用方下一轮重试语义）
	if err := quarantineRemove(dir); err == nil {
		t.Fatalf("不存在目录应报错")
	}
}

func TestCacheDirTaskIDForms(t *testing.T) {
	if id, ok := cacheDirTaskID("uploads-gw-abc"); !ok || id != "gw-abc" {
		t.Fatalf("上传型解析: %q %v", id, ok)
	}
	if id, ok := cacheDirTaskID("gw-abc"); !ok || id != "gw-abc" {
		t.Fatalf("repo 型解析: %q %v", id, ok)
	}
}

// ---- F3 三级重物化（ADR-225 §4.2）：层②树 tar / 层③上传原件 + scratch 清理 ----

// dlObject — DownloadFile 可服务的对象（file_id → 路径名+字节）。
type dlObject struct {
	name string
	data []byte
}

func (f *fakeStorageServer) DownloadFile(req *pb.DownloadFileRequest, stream pb.StorageService_DownloadFileServer) error {
	obj, ok := f.objects[req.GetFileId()]
	if !ok {
		return os.ErrNotExist
	}
	for i := 0; i < len(obj.data); i += 64<<10 + 1 {
		e := i + 64<<10 + 1
		if e > len(obj.data) {
			e = len(obj.data)
		}
		if err := stream.Send(&pb.DownloadFileChunk{Data: obj.data[i:e]}); err != nil {
			return err
		}
	}
	return nil
}

func tarGzOf(t *testing.T, dir string) []byte {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "t.tar.gz")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := writeTreeTarGz(f, dir); err != nil {
		t.Fatalf("pack: %v", err)
	}
	f.Close()
	b, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func zipOf(t *testing.T, dir string, names ...string) []byte {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "t.zip")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, n := range names {
		w, werr := zw.Create(n)
		if werr != nil {
			t.Fatalf("zip entry: %v", werr)
		}
		b, _ := os.ReadFile(filepath.Join(dir, n))
		_, _ = w.Write(b)
	}
	zw.Close()
	f.Close()
	b, _ := os.ReadFile(tmp)
	return b
}

func runIncOn(t *testing.T, s *TaskServiceImpl, taskID, cur string) *orchestrator.IncrementalContext {
	t.Helper()
	inc := &orchestrator.IncrementalContext{}
	s.incrementalCtx[taskID] = inc
	s.runIncrementalDiff(context.Background(), taskID, cur)
	return inc
}

func TestRunIncrementalDiff_RehydrateLevel2TreeTar(t *testing.T) {
	// 基线卷树不在位 + 树 tar 在桶（S5 锚点）→ 层②重物化参与 diff，事后 scratch 清理
	baseTree := mustDir(t, map[string]string{"keep.py": "k=1", "mod.py": "m=1"})
	fake := &fakeStorageServer{mode: "s3", fileOK: map[string]bool{}, objects: map[string]dlObject{
		"tar-base": {name: "trees/base-9.tar.gz", data: tarGzOf(t, baseTree)},
	}}
	addr := startFakeStorage(t, fake)
	t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
	s := newIncrementalSvc(t, t.TempDir())
	s.tasks["base-9"] = &pb.ScanTask{TaskId: "base-9", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now()),
		Config: map[string]string{"tree_tar_file_id": "tar-base"}}
	cur := mustDir(t, map[string]string{"keep.py": "k=1", "mod.py": "m=2", "add.py": "a=1"})
	s.tasks["inc-9"] = &pb.ScanTask{TaskId: "inc-9", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING, Config: map[string]string{"incremental": "true"}}

	inc := runIncOn(t, s, "inc-9", cur)
	active, baseline, changed, deleted, _ := inc.Snapshot()
	if !active || baseline != "base-9" {
		t.Fatalf("层②重物化未激活: active=%v baseline=%q", active, baseline)
	}
	if strings.Join(changed, ",") != "add.py,mod.py" || len(deleted) != 0 {
		t.Fatalf("tar 层 diff: changed=%v deleted=%v", changed, deleted)
	}
	// A3.2 scratch 清理断言：重物化目录用毕即清
	if _, err := os.Stat(filepath.Join(s.reposDir, "rehy-inc-9-base-9")); !os.IsNotExist(err) {
		t.Fatalf("重物化 scratch 未清理: %v", err)
	}
}

func TestRunIncrementalDiff_RehydrateLevel3UploadArchive(t *testing.T) {
	// 无卷树无树 tar → 层③上传原件重解包+剥壳（zip 内含一层壳目录）
	inner := mustDir(t, map[string]string{"shell-x/keep.py": "k=1", "shell-x/del.py": "d=1"})
	fake := &fakeStorageServer{mode: "s3", fileOK: map[string]bool{}, objects: map[string]dlObject{
		"up-base": {name: "uploads/up-1.zip", data: zipOf(t, inner, "shell-x/keep.py", "shell-x/del.py")},
	}}
	addr := startFakeStorage(t, fake)
	t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
	s := newIncrementalSvc(t, t.TempDir())
	s.tasks["base-A"] = &pb.ScanTask{TaskId: "base-A", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_COMPLETED, CreatedAt: ts2pb(time.Now()),
		Config: map[string]string{"upload_file_id": "up-base"}}
	cur := mustDir(t, map[string]string{"keep.py": "k=1"})
	s.tasks["inc-A"] = &pb.ScanTask{TaskId: "inc-A", ProjectId: "p1",
		Status: pb.TaskStatus_TASK_STATUS_RUNNING, Config: map[string]string{"incremental": "true"}}

	inc := runIncOn(t, s, "inc-A", cur)
	active, baseline, changed, deleted, _ := inc.Snapshot()
	if !active || baseline != "base-A" {
		t.Fatalf("层③重物化未激活: active=%v baseline=%q", active, baseline)
	}
	// 剥壳对齐：shell-x/ 剥掉后 del.py 直接对位（A4.3 同款口径）
	if strings.Join(deleted, ",") != "del.py" || len(changed) != 0 {
		t.Fatalf("上传原件层 diff（剥壳对齐）: changed=%v deleted=%v", changed, deleted)
	}
	if _, err := os.Stat(filepath.Join(s.reposDir, "rehy-inc-A-base-A")); !os.IsNotExist(err) {
		t.Fatalf("层③ scratch 未清理: %v", err)
	}
}

// R52（2026-09-11 审计修复批次）: 容量线记账必须用驱逐前体积——此前删后 dirSize 恒 0，
// total 永不下降→水位 break 失效→超线即逐出全部候选（而非驱至 90% 水位）。
func TestRepoCacheGC_CapacityStopsAtWatermark(t *testing.T) {
	addr := startFakeStorage(t, &fakeStorageServer{mode: "s3", fileOK: map[string]bool{"tar-1": true, "tar-2": true}})
	t.Setenv("CODEAUDIT_STORAGE_ADDR", addr)
	s, g := newGC(t, t.TempDir())
	// 两树均在 TTL 窗口内（<24h）——驱逐只由容量线驱动（否则 TTL 线先删，容量线测不到）
	dirOld := mkTaskTree(t, s, "cap-old", pb.TaskStatus_TASK_STATUS_COMPLETED, 2*time.Hour, "tar-1")
	if err := os.WriteFile(filepath.Join(dirOld, "big.py"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	dirNew := mkTaskTree(t, s, "cap-new", pb.TaskStatus_TASK_STATUS_COMPLETED, 1*time.Hour, "tar-2")
	oldB, newB := dirSize(filepath.Dir(dirOld)), dirSize(filepath.Dir(dirNew))
	// maxBytes 夹在「单棵 old」与「双树之和」之间：修复后逐 old 即达 90% 水位停手；
	// 变异（记账恒 0）时水位永不达 → 连 new 一起逐出 → 本测试判杀
	if newB <= 0 || oldB <= newB {
		t.Fatalf("fixture degenerate: oldB=%d newB=%d", oldB, newB)
	}
	g.maxBytes = oldB + newB - 1 // 恒 < 双树总量=必超线；old 驱逐后即达 90% 水位
	g.tick()
	if _, err := os.Stat(dirOld); !os.IsNotExist(err) {
		t.Fatalf("older tree should be evicted first (old→new), stat err=%v", err)
	}
	if _, err := os.Stat(dirNew); err != nil {
		t.Fatalf("newer tree must survive once watermark reached (R52 记账修复): %v", err)
	}
}
