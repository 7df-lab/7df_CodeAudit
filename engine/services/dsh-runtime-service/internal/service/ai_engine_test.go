package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/codeaudit/proto-gen"
)

// R56（2026-09-11 报障修复）: 沙箱不可达走 RuleScan 兜底时，响应必须置 degraded=true——
// 此前降级路径 success 返回且无标志，编排置阶段 done:ai、前端绿色对勾，用户误以为 AI 真跑完。
//
// 密封性（2026-09-13 门禁修复）：本用例此前依赖本机 sim 栈的 result-service(50058) 接收
// BatchCreateFindings（ADR-134 落盘错误上抛，无后端即整链报错）且会打到真实内网 manager——
// sim 栈停即红。现 manager 钉不可达环回（OPENSHELL_MANAGER_URL 为 managerEndpoint 解析序
// 最高优先），result 用进程内 gRPC 假后端，纯离线可复现。

// fakeResultBackend — ResultService 最小假实现：只应答 BatchCreateFindings（落盘成功）。
type fakeResultBackend struct {
	pb.UnimplementedResultServiceServer
}

func (f *fakeResultBackend) BatchCreateFindings(ctx context.Context, req *pb.BatchCreateFindingsRequest) (*pb.BatchCreateFindingsResponse, error) {
	ids := make([]string, 0, len(req.GetFindings()))
	for i := range req.GetFindings() {
		ids = append(ids, fmt.Sprintf("f-deg-%d", i))
	}
	return &pb.BatchCreateFindingsResponse{FindingIds: ids}, nil
}

func startFakeResultBackend(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterResultServiceServer(srv, &fakeResultBackend{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestRunFiveAgentPipeline_DegradedFlag(t *testing.T) {
	t.Setenv("OPENSHELL_MANAGER_URL", "http://127.0.0.1:1") // 环回不可达：launch fail-loud 走降级
	t.Setenv("CODEAUDIT_RESULT_ADDR", startFakeResultBackend(t)) // 构造时读取（ai_engine.go），须先于 NewDSHRuntimeService
	s := NewDSHRuntimeService()
	dir := t.TempDir()
	src := "import sqlite3\ndef get_user(uid):\n    conn = sqlite3.connect('app.db')\n    cur = conn.cursor()\n    cur.execute('SELECT * FROM users WHERE id = \"%s\"' % uid)\n    return cur.fetchone()\nAPI_TOKEN = 'hunter2-hardcoded-secret'\n"
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &pb.RunAIAnalysisRequest{TaskId: "t-deg-r56", ProjectPath: dir}
	resp, err := s.runFiveAgentPipeline(context.Background(), req, "sess-unreachable-r56")
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !resp.GetDegraded() {
		t.Fatal("sandbox unreachable must set degraded=true (R56)")
	}
	if resp.GetResult().GetAiFindingsCount() == 0 {
		t.Fatal("RuleScan fallback should produce findings for vulnerable sample")
	}
}
