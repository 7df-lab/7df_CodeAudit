package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// R54（2026-09-11 修复批次）: QueryCPG 只读 <project>/.codeaudit/cpg.json 形态——
// 其余路径一律拒绝（此前 os.ReadFile 任意请求路径=任意文件读取面）。
func TestQueryCPG_RejectsNonCpgPaths(t *testing.T) {
	s := newCodeAnalysisService()
	for _, p := range []string{"/etc/passwd", "/data/secret.json", "relative.json", "../../etc/shadow"} {
		resp, err := s.QueryCPG(context.Background(), cpgReq(p))
		if err != nil {
			t.Fatalf("QueryCPG(%q): %v", p, err)
		}
		if !strings.Contains(resp.GetResultJson(), "rejected") {
			t.Fatalf("path %q must be rejected (R54), got: %s", p, resp.GetResultJson())
		}
	}
	// 合法形态正常读取
	dir := t.TempDir()
	cpgDir := filepath.Join(dir, ".codeaudit")
	if err := os.MkdirAll(cpgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cpgDir, "cpg.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := s.QueryCPG(context.Background(), cpgReq(filepath.Join(cpgDir, "cpg.json")))
	if err != nil {
		t.Fatalf("QueryCPG legal path: %v", err)
	}
	if !strings.Contains(resp.GetResultJson(), `"ok":true`) {
		t.Fatalf("legal cpg path must read through, got: %s", resp.GetResultJson())
	}
}

func cpgReq(path string) *pb.QueryCPGRequest {
	return &pb.QueryCPGRequest{CpgStoragePath: path}
}
