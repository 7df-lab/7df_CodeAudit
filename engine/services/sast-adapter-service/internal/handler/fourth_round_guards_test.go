package handler

// 第四轮审计锁定测试三件（sast-adapter 面）：
//  1. P1 persistToResult 必须把 FailedCount>0 当落库失败——result 侧逐条 Create
//     失败只计数不上错误，nil error 会让上层把响应对入幂等缓存（R81 意图被部分
//     失败路径绕过），丢失的那批 findings 永不重试。
//  2. P1 fusion resolveFindings 必须返回克隆——各 stage 原地改写共享缓存指针=
//     跨请求竞争 + 二次融合不可复现（R48/ADR-128 同类）。
//  3. P2 changed_files 进 argv 前消毒——前导 "-"=选项注入（R74 同类），".." 段/
//     绝对路径=项目根逃逸。

import (
	"context"
	"net"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

// ---- 1. FailedCount ----

type failedCountResultBackend struct {
	pb.UnimplementedResultServiceServer
	srv *grpc.Server
}

func (b *failedCountResultBackend) BatchCreateFindings(ctx context.Context, req *pb.BatchCreateFindingsRequest) (*pb.BatchCreateFindingsResponse, error) {
	return &pb.BatchCreateFindingsResponse{
		FindingIds:  []string{"f-ok-1"}, // 部分成功
		FailedCount: 2,                  // 部分失败：proto 语义=不上 RPC 错误
	}, nil
}

func TestPersistToResult_FailedCountIsError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &failedCountResultBackend{srv: grpc.NewServer()}
	pb.RegisterResultServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)

	findings := []*pb.UnifiedFinding{
		{FindingId: "f-ok-1"}, {FindingId: "f-fail-1"}, {FindingId: "f-fail-2"},
	}
	_, perr := persistToResult(lis.Addr().String(), "t-1", "req-1", findings)
	if perr == nil {
		t.Fatalf("FailedCount=2 须视同落库失败——否则响应对入幂等缓存，丢失的 findings 永不重试（R81 旁路）")
	}
	if !strings.Contains(perr.Error(), "failed to persist") {
		t.Fatalf("错误应锚定 failed_count 语义: %v", perr)
	}
}

// ---- 2. fusion clone ----

func TestResolveFindings_ReturnsCloneNotCachePointer(t *testing.T) {
	h := &SASTFusionHandler{}
	pristine := &pb.UnifiedFinding{FindingId: "f-1"}
	h.findingStore.Store("f-1", pristine)

	out1, missing := h.resolveFindings([]string{"f-1"})
	if len(missing) != 0 || len(out1) != 1 {
		t.Fatalf("resolve: out=%d missing=%v", len(out1), missing)
	}
	// 模拟 fusion stage 的原地改写（stage_merge: MatchedFindings append / IsUnique 翻转）
	out1[0].MatchedFindings = append(out1[0].GetMatchedFindings(), "f-other")
	out1[0].IsUnique = true

	out2, _ := h.resolveFindings([]string{"f-1"})
	if len(out2[0].GetMatchedFindings()) != 0 || out2[0].GetIsUnique() {
		t.Fatalf("二次解析拿到被首轮污染的缓存实体（共享指针改写）——matched=%v unique=%v",
			out2[0].GetMatchedFindings(), out2[0].GetIsUnique())
	}
	if out2[0] == pristine {
		t.Fatalf("返回值不得是缓存本体指针")
	}
}

// ---- 3. changed_files 消毒 ----

func TestValidateChangedFiles(t *testing.T) {
	for _, bad := range []string{
		"--config=x",         // 选项注入（R74 同类）
		"-o",                 // 前导短横线
		"../etc/passwd",      // 穿越段
		"a/../../b",          // 混合穿越
		"/etc/passwd",        // 绝对路径
		`..\windows\win.ini`, // 反斜杠穿越（归一后判段）
	} {
		if err := validateChangedFiles([]string{"src/a.py", bad}); err == nil {
			t.Fatalf("changed_files %q 应被拒绝", bad)
		}
	}
	// 合法面：相对路径、含 "foo.." 前缀的文件名（非穿越段）不受误伤
	for _, ok := range []string{"src/a.py", "foo..bar.txt", "dir.sub/x-y_1.go"} {
		if err := validateChangedFiles([]string{ok}); err != nil {
			t.Fatalf("合法清单项 %q 被误拒: %v", ok, err)
		}
	}
	// "pkg/../pkg/b.go" 含 ".." 段必须拒绝（段级判定，非子串误杀）
	if err := validateChangedFiles([]string{"pkg/../pkg/b.go"}); err == nil {
		t.Fatalf("含 '..' 段的清单项应被拒绝")
	}
}
