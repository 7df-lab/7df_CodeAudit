package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baton/internal/project"
)

const richFixture = "testdata/tasklog_rich"

func render(t *testing.T, format string, tail int) string {
	t.Helper()
	s, err := Render(Options{InRoot: richFixture, Format: format, Tail: tail})
	if err != nil {
		t.Fatalf("Render(%s) 失败: %v", format, err)
	}
	return s
}

// golden 双方言黄金：档案无时间源注入，同档必出同文本（确定性锁定）。
func TestGoldenRich(t *testing.T) {
	for _, c := range []struct{ format, golden string }{
		{"maka", "testdata/golden_rich_maka.md"},
		{"dsh", "testdata/golden_rich_dsh.md"},
	} {
		got := render(t, c.format, 20)
		if os.Getenv("BATON_UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(c.golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(c.golden)
		if err != nil {
			t.Fatalf("读黄金 %s（先 BATON_UPDATE_GOLDEN=1 生成）: %v", c.golden, err)
		}
		if got != string(want) {
			t.Errorf("%s 与黄金不一致\n--- got ---\n%s\n--- want ---\n%s", c.format, got, string(want))
		}
	}
}

// 方言只差包装层级：maka 自包含全文 H1 起；dsh 任务卡前置段 H2 起、小节降一级。
func TestDialectLevels(t *testing.T) {
	maka := render(t, "maka", 20)
	dsh := render(t, "dsh", 20)
	if !strings.HasPrefix(maka, "# baton 交接简报\n") {
		t.Errorf("maka 应以 H1 开头，got %.30q", maka)
	}
	if !strings.HasPrefix(dsh, "## baton 交接简报（前任进度）\n") {
		t.Errorf("dsh 应以 H2 开头，got %.30q", dsh)
	}
	if strings.Contains(dsh, "\n# ") {
		t.Error("dsh 前置段内不得出现 H1（避免与任务卡结构冲突）")
	}
	if !strings.Contains(maka, "\n## 目标\n") || !strings.Contains(dsh, "\n### 目标\n") {
		t.Error("小节层级应随方言降级")
	}
}

// 富档案语义：脊柱全段、机械状态段、公理落点逐项可见。
func TestRichSemantics(t *testing.T) {
	s := render(t, "maka", 20)
	for _, want := range []string{
		"- task: T-1042",
		"- workspace: 22222222-2222-4222-8222-222222222222（git anchor: abc123def456）",
		"- 前任: dsh / deepseek-chat-v3.2",
		"- 档案: 13 条事件（seq 1–13），截至 2026-09-17T10:04:23Z",
		"修复订单回调重复扣款",
		"- 不动支付网关SDK",
		"（evidence: seq 6, seq 7）",
		"- [adopted] D-1",
		"- [rejected] D-2 升级支付网关 SDK 由其统一去重",
		"- 幂等键存 Redis 还是 DB 尚未定",
		"待网关日志证实",
		"先写幂等键单测",
		"PAUSED — handoff freeze",
		"- main-t2-c1 run_check",
		"- 请求: dsh/deepseek-chat-v3.2 → maka（模型供应商切换演练，发起: xiaowang）",
		"- 已完成: 0 次",
		"args_ref\":\"blob:18ca51bc376adb68", // 尾部逐字不改写
		"outcome\":\"indeterminate",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("简报缺少关键内容: %q", want)
		}
	}
	if strings.Contains(s, "首次换手") {
		t.Error("含 handoff 事件的档案不得标'首次换手'")
	}
}

func TestTailControl(t *testing.T) {
	short := render(t, "maka", 3)
	if got := strings.Count(short, `"seq":`); got != 3 {
		t.Errorf("tail=3 应只含 3 条事件行，got %d", got)
	}
	if !strings.Contains(short, "最近 3 条") {
		t.Error("尾部段标题应标注实际条数")
	}
	if s := render(t, "maka", 0); strings.Contains(s, "尾部事件") {
		t.Error("tail=0 应整段省略尾部")
	}
}

// 链路测试：M2 投影产物直接作为 M3 输入（真实数据流，非夹具拼装）。
// 贫 spine（v0 浅投影）下空段省略、首换手标注——诚实反映档案现状。
func TestChainFromProjector(t *testing.T) {
	out := t.TempDir()
	opts := project.Options{
		SSEPath: "../project/testdata/basic.sse.log", OutRoot: out,
		Task: "T-1042", Goal: "修复订单回调重复扣款",
		Constraints: []string{"不动支付网关SDK"},
		WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Now:         time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC),
	}
	if _, err := project.Run(opts); err != nil {
		t.Fatal(err)
	}
	s, err := Render(Options{InRoot: out, Format: "maka", Tail: 20})
	if err != nil {
		t.Fatalf("投影产物渲染失败: %v", err)
	}
	for _, want := range []string{"修复订单回调重复扣款", "- 不动支付网关SDK", "首次换手（档案无 handoff 事件）"} {
		if !strings.Contains(s, want) {
			t.Errorf("链路简报缺少: %q", want)
		}
	}
	for _, absent := range []string{"已验证事实", "## 决策", "需复核调用", "## 任务状态"} {
		if strings.Contains(s, absent) {
			t.Errorf("贫 spine 空段应整段省略: %q", absent)
		}
	}
}

func TestBadFormatAndArchive(t *testing.T) {
	if _, err := Render(Options{InRoot: richFixture, Format: "foo"}); err == nil {
		t.Error("未知方言应报错")
	}
	if _, err := Render(Options{InRoot: t.TempDir(), Format: "maka"}); err == nil ||
		!strings.Contains(err.Error(), "events.jsonl") {
		t.Errorf("空档案应报错, got %v", err)
	}
	// 缺 WORKSPACE.json：身份标记缺失 = 档案不完整，硬闸而非静默渲染
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".tasklog"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"events.jsonl", "spine.json"} {
		b, err := os.ReadFile(filepath.Join(richFixture, ".tasklog", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".tasklog", name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Render(Options{InRoot: dir, Format: "maka"}); err == nil ||
		!strings.Contains(err.Error(), "WORKSPACE.json") {
		t.Errorf("缺 WORKSPACE.json 应硬闸, got %v", err)
	}
}

// 逐字节防线：事件行含 ``` 时围栏升级，简报 Markdown 不被撞击。
func TestFenceSafety(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".tasklog"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"seq":1,"ts":"2026-09-17T10:00:00Z","task":"T","pen":{"agent":"dsh"},"type":"model.message","message":{"role":"user","parts":[{"kind":"text","text":"代码块` + "```" + `撞击"}]}}`
	files := map[string]string{
		"events.jsonl":   line + "\n",
		"spine.json":     `{"task":"T","updated_at_seq":1}`,
		"WORKSPACE.json": `{"workspace_id":"w","task":"T","created_ts":"2026-09-17T10:00:00Z","parent_workspace_id":null}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, ".tasklog", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Render(Options{InRoot: dir, Format: "maka", Tail: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "````jsonl") {
		t.Error("含 ``` 的事件行应触发四级围栏")
	}
	if !strings.Contains(s, "代码块```撞击") {
		t.Error("逐字内容不得被改写")
	}
}
