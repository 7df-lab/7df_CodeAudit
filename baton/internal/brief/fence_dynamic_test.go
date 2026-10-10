package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 围栏动态定长：尾部行含 4+ 连续反引号时围栏 ≥ run+1（单级升级会被撞破）。
func TestFenceDynamicLength(t *testing.T) {
	root := t.TempDir()
	tl := filepath.Join(root, ".tasklog")
	if err := os.MkdirAll(tl, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"seq":1,"ts":"2026-09-20T00:00:00Z","task":"T-1","pen":{"agent":"dsh"},"type":"model.message","message":{"role":"assistant","parts":[{"kind":"text","text":"代码 ` + strings.Repeat("`", 4) + `python\nprint(1)\n` + strings.Repeat("`", 4) + ` 结束"}]}}`
	files := map[string]string{
		"events.jsonl":   line + "\n",
		"spine.json":     `{"task":"T-1","updated_at_seq":1}`,
		"WORKSPACE.json": `{"workspace_id":"w","task":"T-1","created_ts":"2026-09-20T00:00:00Z"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tl, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	text, err := Render(Options{InRoot: root, Format: "maka", Tail: 20})
	if err != nil {
		t.Fatal(err)
	}
	fence := strings.Repeat("`", 5) + "jsonl"
	if !strings.Contains(text, fence) {
		tail := text
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		t.Fatalf("内容含 4 连反引号，围栏应升到 ≥5: \n%s", tail)
	}
	// 围栏闭合存在（不因内容撞击提前闭合）：恰一对 5+ 反引号围栏行
	if strings.Count(text, strings.Repeat("`", 5)) < 2 {
		t.Fatalf("围栏未成对: %s", text)
	}
}
