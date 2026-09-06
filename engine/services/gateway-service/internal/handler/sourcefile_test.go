package handler

// ADR-195 回归锁：任务源码全文端点——根解析四流（repo/link/project_config/唯一内容
// 回退）、根内路径回退（exact>suffix>basename）、穿越/软链/标记文件拒绝、二进制与
// 超限拒绝。进程内真实 gRPC 后端（同 transcode_test.go 口径，不 mock HTTP 层）。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

type srcBackend struct {
	pb.UnimplementedTaskServiceServer
	pb.UnimplementedProjectServiceServer
	addr string
	srv  *grpc.Server
}

func startSrcBackend(t *testing.T) *srcBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &srcBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterTaskServiceServer(b.srv, b)
	pb.RegisterProjectServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *srcBackend) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	return &pb.ScanTask{TaskId: req.GetTaskId(), ProjectId: "proj-1"}, nil
}

func (b *srcBackend) GetProjectConfig(ctx context.Context, req *pb.GetProjectConfigRequest) (*pb.ProjectConfig, error) {
	return &pb.ProjectConfig{ProjectId: req.GetProjectId(), Config: projectCfgForTest}, nil
}

// projectCfgForTest — 由测试用例注入（project_config 流）
var projectCfgForTest map[string]string

func newSrcTranscoder(t *testing.T, b *srcBackend) *Transcoder {
	tr := NewTranscoder(BackendAddrs{TaskAddr: b.addr, ProjectAddr: b.addr, CallTimeoutS: 5})
	t.Cleanup(tr.Close)
	return tr
}

// tree — 造一棵最小源树：up1/up2 两个上传目录 + repo 任务目录
func tree(t *testing.T) (uploads, up1, up2, repo string) {
	t.Helper()
	root := t.TempDir()
	uploads = filepath.Join(root, "uploads")
	up1 = filepath.Join(uploads, "up-aaa")
	up2 = filepath.Join(uploads, "up-bbb")
	repo = filepath.Join(root, "repos", "t-repo")
	for _, f := range []struct{ dir, name, body string }{
		{up1, "src/main/java/Foo.java", "package a;\nclass Foo {\n  int leak() { return 1; }\n}\n"},
		{up1, "src/main/java/util/Foo.java", "package util;\nclass Foo {}\n"},
		{up1, "Conf.java", "conf v1\n"},
		{up2, "src/main/java/Foo.java", "package b;\nclass FooNewer { int x; }\n"},
		{up2, "Bin.dat", "\x00\x01binary"},
		{repo, "svc/handler.py", "def h():\n    pass\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.dir, f.name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return uploads, up1, up2, repo
}

func setSourceDirs(t *testing.T, uploads, repos string) {
	t.Helper()
	oldU, oldR := UploadsDir, ReposDir
	UploadsDir, ReposDir = uploads, repos
	t.Cleanup(func() { UploadsDir, ReposDir = oldU, oldR })
}

func getJSON(t *testing.T, srv *httptest.Server, path string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestSourceFile_UploadLinkFlow(t *testing.T) {
	uploads, up1, _, _ := tree(t)
	// link 文件指向 up1（CreateScanTask 拦截产物）
	if err := os.WriteFile(filepath.Join(up1, taskLinkName("t-link")), []byte("t-link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	setSourceDirs(t, uploads, "")
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	code, m := getJSON(t, srv, "/v1/tasks/t-link/source-file?path=src/main/java/Foo.java")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, m)
	}
	if m["root_via"] != "upload_link" || m["resolved_via"] != "exact" {
		t.Fatalf("root_via=%v resolved_via=%v", m["root_via"], m["resolved_via"])
	}
	if !strings.Contains(m["content"].(string), "class Foo {") || m["total_lines"].(float64) != 5 {
		t.Fatalf("content/lines mismatch: %v", m)
	}
}

func TestSourceFile_BasenameAndSuffixFallback(t *testing.T) {
	uploads, up1, _, _ := tree(t)
	if err := os.WriteFile(filepath.Join(up1, taskLinkName("t-link")), []byte("t-link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	setSourceDirs(t, uploads, "")
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	// 裸文件名（AI 结论常见形态）：up1 内有两个 Foo.java，取路径最短者（主源文件口径）
	code, m := getJSON(t, srv, "/v1/tasks/t-link/source-file?path=Foo.java")
	if code != http.StatusOK || m["resolved_via"] != "basename" {
		t.Fatalf("basename fallback: code=%d via=%v", code, m["resolved_via"])
	}
	if m["path"] != "src/main/java/Foo.java" {
		t.Fatalf("want shortest match src/main/java/Foo.java, got %v", m["path"])
	}
	// 截断路径（后缀匹配）
	code, m = getJSON(t, srv, "/v1/tasks/t-link/source-file?path=java/util/Foo.java")
	if code != http.StatusOK || m["resolved_via"] != "suffix" || m["path"] != "src/main/java/util/Foo.java" {
		t.Fatalf("suffix fallback: code=%d m=%v", code, m)
	}
}

func TestSourceFile_RepoDirAndProjectConfigFlows(t *testing.T) {
	uploads, up1, _, repo := tree(t)
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	setSourceDirs(t, uploads, filepath.Dir(repo))
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	// repo 流：repos_dir/<task_id>
	code, m := getJSON(t, srv, "/v1/tasks/t-repo/source-file?path=svc/handler.py")
	if code != http.StatusOK || m["root_via"] != "repos_dir" {
		t.Fatalf("repo flow: code=%d via=%v", code, m["root_via"])
	}
	// project_config 流（ADR-148）：task 无 repo 目录、无 link → 查项目配置
	projectCfgForTest = map[string]string{"project_path": up1}
	t.Cleanup(func() { projectCfgForTest = nil })
	code, m = getJSON(t, srv, "/v1/tasks/t-cfg/source-file?path=Conf.java")
	if code != http.StatusOK || m["root_via"] != "project_config" {
		t.Fatalf("project_config flow: code=%d via=%v m=%v", code, m["root_via"], m)
	}
}

func TestSourceFile_ContentFallbackForLegacyTasks(t *testing.T) {
	uploads, _, up2, _ := tree(t)
	// up2 的 Foo.java 更新时间更晚（唯一内容回退取最新）
	if err := os.Chtimes(filepath.Join(up2, "src/main/java/Foo.java"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	projectCfgForTest = map[string]string{} // 无 project_path
	t.Cleanup(func() { projectCfgForTest = nil })
	setSourceDirs(t, uploads, "")
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	code, m := getJSON(t, srv, "/v1/tasks/t-legacy/source-file?path=src/main/java/Foo.java")
	if code != http.StatusOK || m["root_via"] != "upload_content_newest" {
		t.Fatalf("content fallback: code=%d via=%v", code, m["root_via"])
	}
	if !strings.Contains(m["content"].(string), "FooNewer") {
		t.Fatalf("want newest upload content, got %v", m["content"])
	}
	// 无任何根可解析 → 404 诚实报错
	code, m = getJSON(t, srv, "/v1/tasks/t-legacy/source-file?path=NoSuch.java")
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestSourceFile_Rejects(t *testing.T) {
	uploads, up1, _, _ := tree(t)
	if err := os.WriteFile(filepath.Join(up1, taskLinkName("t-link")), []byte("t-link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 软链逃逸：up1 内 symlink 指向根外文件
	outside := filepath.Join(filepath.Dir(uploads), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(up1, "escape.java")); err != nil {
		t.Fatal(err)
	}
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	setSourceDirs(t, uploads, "")
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	// 穿越
	if code, _ := getJSON(t, srv, "/v1/tasks/t-link/source-file?path=../../../secret.txt"); code != http.StatusNotFound {
		t.Fatalf("traversal: want 404, got %d", code)
	}
	// 软链逃逸（基名回退命中软链 → accept 拒绝 → 无候选）
	if code, _ := getJSON(t, srv, "/v1/tasks/t-link/source-file?path=escape.java"); code != http.StatusNotFound {
		t.Fatalf("symlink escape: want 404, got %d", code)
	}
	// 标记文件自身不可读
	if code, _ := getJSON(t, srv, "/v1/tasks/t-link/source-file?path=.codeaudit-task-t-link"); code != http.StatusNotFound {
		t.Fatalf("marker file: want 404, got %d", code)
	}
	// 缺参
	if code, _ := getJSON(t, srv, "/v1/tasks/t-link/source-file"); code != http.StatusBadRequest {
		t.Fatalf("missing path: want 400, got %d", code)
	}
}

func TestSourceFile_BinaryRejected(t *testing.T) {
	uploads, _, up2, _ := tree(t)
	if err := os.WriteFile(filepath.Join(up2, taskLinkName("t-bin")), []byte("t-bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := startSrcBackend(t)
	tr := newSrcTranscoder(t, b)
	setSourceDirs(t, uploads, "")
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	if code, _ := getJSON(t, srv, "/v1/tasks/t-bin/source-file?path=Bin.dat"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("binary: want 415, got %d", code)
	}
}

func TestWriteTaskLink(t *testing.T) {
	uploads, up1, _, _ := tree(t)
	setSourceDirs(t, uploads, "")

	writeTaskLink("t-9", filepath.Join(filepath.Dir(uploads), "elsewhere")) // 非上传目录 → 不写
	if _, err := os.Stat(filepath.Join(filepath.Dir(uploads), "elsewhere", taskLinkName("t-9"))); !os.IsNotExist(err) {
		t.Fatal("non-upload path must not create link")
	}
	writeTaskLink("t-9", up1)
	if _, err := os.Stat(filepath.Join(up1, taskLinkName("t-9"))); err != nil {
		t.Fatalf("link not written: %v", err)
	}
	writeTaskLink("", up1) // 空 task_id 防御
	writeTaskLink("t-a", "")
}
