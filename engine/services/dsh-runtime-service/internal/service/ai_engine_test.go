package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)


// R56（2026-09-11 报障修复）: 沙箱不可达走 RuleScan 兜底时，响应必须置 degraded=true——
// 此前降级路径 success 返回且无标志，编排置阶段 done:ai、前端绿色对勾，用户误以为 AI 真跑完。
func TestRunFiveAgentPipeline_DegradedFlag(t *testing.T) {
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
