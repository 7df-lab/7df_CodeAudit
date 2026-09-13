package handler

// R79 锁定测试（结构性守卫）：重物化临时目录必须唯一命名——共享 `<id>.tmp` 在并发
// miss 下被入口/收尾 RemoveAll 互拆，可产出"缺文件但 stat 存在"的投毒缓存树。

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/codeaudit/proto-gen"
)

func TestRehydrateTmpDir_UniqueSuffix(t *testing.T) {
	src, err := os.ReadFile("sourcefile_rehydrate.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `taskDir + ".tmp"`) {
		t.Fatal("R79: 共享 .tmp 回潮（并发 miss 互拆/缓存投毒）")
	}
	if !strings.Contains(string(src), `".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)`) {
		t.Fatal("R79: tmp 唯一后缀缺失")
	}
}

// R85（复审）: 阈值过滤纯函数——超阈值的 .tmp-* 入选，新鲜残留不入选。
func TestSweepStaleRehydrateTmp_RemovesOnlyAged(t *testing.T) {
	base := t.TempDir()
	taskDir := filepath.Join(base, "t-1")
	stale := taskDir + ".tmp-111"
	fresh := taskDir + ".tmp-222"
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	got := staleRehydrateTmpDirs(taskDir)
	if len(got) != 1 || got[0] != stale {
		t.Fatalf("stale dirs = %v, want [%s]", got, stale)
	}
}

// R86: 孤儿清扫的终态守卫——任务在途（RUNNING）或状态查不到时一律不扫，
// 防清掉正常业务的在途残留；任务已终态（DEAD）时超阈值孤儿才被移除。
func TestSweepStaleRehydrateTmp_TerminalGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    pb.TaskStatus
		getScanErr error
		wantSweep bool
	}{
		{"任务在途 RUNNING → 不扫", pb.TaskStatus_TASK_STATUS_RUNNING, nil, false},
		{"任务已终态 DEAD → 扫", pb.TaskStatus_TASK_STATUS_DEAD, nil, true},
		{"任务已终态 COMPLETED → 扫", pb.TaskStatus_TASK_STATUS_COMPLETED, nil, true},
		{"状态查不到（task-service 不可达）→ 保守不扫", 0, status.Error(codes.Unavailable, "down"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeStreamTask{scanStatus: tc.status, getScanErr: tc.getScanErr}
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			gsrv := grpc.NewServer()
			pb.RegisterTaskServiceServer(gsrv, fake)
			go func() { _ = gsrv.Serve(lis) }()
			t.Cleanup(gsrv.Stop)

			tr := NewTranscoder(BackendAddrs{TaskAddr: lis.Addr().String(), CallTimeoutS: 5})
			defer tr.Close()

			taskDir := filepath.Join(t.TempDir(), "t-1")
			stale := taskDir + ".tmp-111"
			fresh := taskDir + ".tmp-222"
			for _, d := range []string{stale, fresh} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			past := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(stale, past, past); err != nil {
				t.Fatal(err)
			}

			tr.sweepStaleRehydrateTmp("t-1", taskDir)

			if tc.wantSweep {
				if _, err := os.Stat(stale); !os.IsNotExist(err) {
					t.Fatalf("终态任务的超阈值孤儿未被清扫: %v", err)
				}
			} else if _, err := os.Stat(stale); err != nil {
				t.Fatalf("非终态/查不到状态时清扫了在途残留（正常业务被清）: %v", err)
			}
			if _, err := os.Stat(fresh); err != nil {
				t.Fatalf("新鲜在途 tmp 被误扫: %v", err)
			}
		})
	}
}
