package service

// ADR-183 diff_patch 服务端校验器回归：
//   - SQL 注入示例（vuln_fix_demo.py 形状，9 行）：@@ 锚点 + 第 6-8 行逐字删除 +
//     3 行参数化新增，产出为工作区逐字重建的规范化补丁（插件引擎 fuzz=0 前提），
//     第 9 行 return cursor.fetchone() 不在删除行中（应用后保留）；
//   - 失配整补丁拒绝（上下文凭记忆改写/文件缺失/路径穿越/Move to）；
//   - 行号漂移仍按内容锚定（头部插入 3 行后同一补丁通过且产出不变）；
//   - 多文件单补丁 / *** End of File 锚定 / Add File / Delete File / 新增行质量归一；
//   - mapSandboxFindings 集成：好补丁填充、坏补丁置空且 finding 保留；
//   - buildFixSuggestion（ai_engine Fix Advisor 出口，ADR-176 4c 债）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/sandbox"
)

// vulnFixDemo — 验收口径的示例工作区（人类任务指令：9 行 SQL 注入，第 6-8 行为漏洞行，
// 第 9 行 return 保留）。
const vulnFixDemo = `import sqlite3


def get_user(conn, user_id):
    cursor = conn.cursor()
    query = "SELECT * FROM users WHERE id = '" + user_id + "'"
    cursor.execute(query)
    # 执行查询并返回结果
    return cursor.fetchone()
`

// vulnFixDemoPatch — LLM 期望形状：@@ 锚点（第 5 行）+ 6-8 行删除 + 3 行参数化新增。
// 注意 @@ 锚点行即 hunk 首条上下文行（Cline apply-patch 同款），故锚点行保留不删。
const vulnFixDemoPatch = `*** Begin Patch
*** Update File: vuln_fix_demo.py
@@     cursor = conn.cursor()
-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"
-    cursor.execute(query)
-    # 执行查询并返回结果
+    query = "SELECT * FROM users WHERE id = ?"
+    cursor.execute(query, (user_id,))
+    # 参数化查询：用户输入不进入 SQL 文本
*** End Patch`

// writeDemoWs — 落地示例工作区，返回目录。
func writeDemoWs(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vuln_fix_demo.py"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNormalizeDiffPatch_SqlInjectionDemo(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	out, err := NormalizeDiffPatch(vulnFixDemoPatch, dir)
	if err != nil {
		t.Fatalf("valid patch rejected: %v", err)
	}
	// 精确全文断言（真实沙箱首跑抓到交错序 bug：@@ 行重复+上下文错位——子串断言不够）：
	// @@ 承载首条上下文行且不重复输出；3 删 3 增按位交错；第 9 行 return 不在删除行。
	want := "*** Begin Patch\n" +
		"*** Update File: vuln_fix_demo.py\n" +
		"@@     cursor = conn.cursor()\n" +
		`-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"` + "\n" +
		"-    cursor.execute(query)\n" +
		"-    # 执行查询并返回结果\n" +
		`+    query = "SELECT * FROM users WHERE id = ?"` + "\n" +
		"+    cursor.execute(query, (user_id,))\n" +
		"+    # 参数化查询：用户输入不进入 SQL 文本\n" +
		"*** End Patch"
	if out != want {
		t.Fatalf("normalized patch mismatch:\n--got--\n%s\n--want--\n%s", out, want)
	}
	// 幂等性：规范化输出再规范化不变（消费端把 @@ 内容作为上下文行）
	again, err := NormalizeDiffPatch(out, dir)
	if err != nil || again != out {
		t.Fatalf("normalization must be idempotent: err=%v\n--again--\n%s", err, again)
	}
	// 行号漂移：工作区头部插入 3 行后，规范化输出仍逐字节一致（内容锚定，与行号无关）
	dir2 := writeDemoWs(t, "# h1\n# h2\n# h3\n"+vulnFixDemo)
	out2, err2 := NormalizeDiffPatch(vulnFixDemoPatch, dir2)
	if err2 != nil || out2 != out {
		t.Fatalf("output must be drift-invariant: err=%v", err2)
	}
}

func TestNormalizeDiffPatch_RejectsRewrittenContext(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	bad := strings.Replace(vulnFixDemoPatch,
		"-    cursor.execute(query)",
		"-    cursor.execute( query )", // 凭记忆改写（多打空格）
		1)
	if _, err := NormalizeDiffPatch(bad, dir); err == nil {
		t.Fatal("rewritten context line must reject the whole patch")
	}
}

func TestNormalizeDiffPatch_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir() // 空目录：目标文件不存在
	if _, err := NormalizeDiffPatch(vulnFixDemoPatch, dir); err == nil {
		t.Fatal("missing target file must reject")
	}
}

func TestNormalizeDiffPatch_RejectsTraversal(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	p := strings.Replace(vulnFixDemoPatch, "*** Update File: vuln_fix_demo.py", "*** Update File: ../../etc/passwd", 1)
	if _, err := NormalizeDiffPatch(p, dir); err == nil {
		t.Fatal("path traversal must reject")
	}
}

func TestNormalizeDiffPatch_RejectsMoveTo(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	p := strings.Replace(vulnFixDemoPatch, "*** End Patch", "*** Move to: renamed.py\n*** End Patch", 1)
	if _, err := NormalizeDiffPatch(p, dir); err == nil {
		t.Fatal("*** Move to: must reject (ADR-183: 插件端无重命名语义)")
	}
}

func TestNormalizeDiffPatch_LineDriftStillAnchors(t *testing.T) {
	// 行号漂移场景：工作区头部插入 3 行后，同一补丁（无行号、纯内容锚定）仍须通过，
	// 且规范化产出与漂移前完全一致（锚定不依赖行号）
	dir := writeDemoWs(t, vulnFixDemo)
	drifted := writeDemoWs(t, "# header line 1\n# header line 2\n# header line 3\n"+vulnFixDemo)
	out1, err1 := NormalizeDiffPatch(vulnFixDemoPatch, dir)
	out2, err2 := NormalizeDiffPatch(vulnFixDemoPatch, drifted)
	if err1 != nil || err2 != nil {
		t.Fatalf("content anchoring must survive line drift: %v / %v", err1, err2)
	}
	if out1 != out2 {
		t.Fatalf("normalized output must be identical under drift:\n--\n%s\n--\n%s", out1, out2)
	}
}

func TestNormalizeDiffPatch_MultiFileSinglePatch(t *testing.T) {
	dir := t.TempDir()
	// 硬编码密钥（代码）+ 调试开关（配置）→ 一个补丁两个 Update File 段
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("import os\n\nTOKEN = \"hardcoded-secret\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.py"), []byte("import os\nDEBUG = True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := `*** Begin Patch
*** Update File: app.py
@@ import os
 
-TOKEN = "hardcoded-secret"
+TOKEN = CodeAudit.secrets.APP_TOKEN
*** Update File: settings.py
@@ import os
-DEBUG = True
+DEBUG = False
*** End Patch`
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("multi-file patch rejected: %v", err)
	}
	if got := strings.Count(out, "*** Update File:"); got != 2 {
		t.Fatalf("must keep both Update File sections, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "+TOKEN = CodeAudit.secrets.APP_TOKEN") || !strings.Contains(out, "+DEBUG = False") {
		t.Fatalf("both sections' changes must survive:\n%s", out)
	}
}

func TestNormalizeDiffPatch_EndOfFileAnchor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tail.py"), []byte("a = 1\nb = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 正例：末行锚定 + End of File 标记（容忍行尾换行的合成空元素）
	ok := `*** Begin Patch
*** Update File: tail.py
@@ b = 2
+c = 3
*** End of File
*** End Patch`
	if _, err := NormalizeDiffPatch(ok, dir); err != nil {
		t.Fatalf("EOF-anchored append must pass: %v", err)
	}
	// 反例：End of File 标记但锚定在首行（不在文件尾）
	bad := `*** Begin Patch
*** Update File: tail.py
@@ a = 1
+c = 3
*** End of File
*** End Patch`
	if _, err := NormalizeDiffPatch(bad, dir); err == nil {
		t.Fatal("*** End of File away from EOF must reject")
	}
}

func TestNormalizeDiffPatch_AddAndDeleteFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.py"), []byte("print('old')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := `*** Begin Patch
*** Add File: newconf.py
+DEBUG = False
+TOKEN_SRC = "env:CODEAUDIT_TOKEN"
*** Delete File: legacy.py
*** End Patch`
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("add+delete patch rejected: %v", err)
	}
	if !strings.Contains(out, "*** Add File: newconf.py") || !strings.Contains(out, "+DEBUG = False") {
		t.Fatalf("add section lost:\n%s", out)
	}
	if !strings.Contains(out, "*** Delete File: legacy.py") {
		t.Fatalf("delete section lost:\n%s", out)
	}
	// Add 目标已存在 → 拒绝
	p2 := strings.Replace(p, "newconf.py", "legacy.py", 1)
	if _, err := NormalizeDiffPatch(p2, dir); err == nil {
		t.Fatal("Add File onto existing target must reject")
	}
	// Delete 目标不存在 → 拒绝
	p3 := strings.Replace(p, "legacy.py", "nonexistent.py", 1)
	if _, err := NormalizeDiffPatch(p3, dir); err == nil {
		t.Fatal("Delete File of missing target must reject")
	}
}

func TestNormalizeDiffPatch_CleanAddedLineQuality(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	// 新增行带弯引号 → 归一为 ASCII 直引号（人类格式规范 §3）
	p := strings.Replace(vulnFixDemoPatch,
		`+    query = "SELECT * FROM users WHERE id = ?"`,
		"+    query = \u201cSELECT * FROM users WHERE id = ?\u201d", 1)
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("quality patch rejected: %v", err)
	}
	if !strings.Contains(out, `+    query = "SELECT * FROM users WHERE id = ?"`) {
		t.Fatalf("smart quotes must be normalized to ASCII:\n%s", out)
	}
}

func TestMapSandboxFindings_FixSuggestionAndDiffPatch(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	good := mapSandboxFindings("t-fix", dir, []sandbox.Finding{{
		Title: "SQL 注入", Severity: "SEVERITY_CRITICAL", CweID: "CWE-89",
		FilePath: "vuln_fix_demo.py", StartLine: 6, Confidence: 0.95,
		Reasoning:     "字符串拼接进 SQL",
		FixSuggestion: "## 修复说明\n改用参数化查询",
		DiffPatch:     vulnFixDemoPatch,
	}})
	if len(good) != 1 {
		t.Fatalf("mapped=%d", len(good))
	}
	if good[0].GetAiFixSuggestion() == "" {
		t.Fatal("ai_fix_suggestion must carry LLM markdown")
	}
	dp := good[0].GetDiffPatch()
	if !strings.HasPrefix(dp, "*** Begin Patch") || !strings.Contains(dp, "@@") {
		t.Fatalf("diff_patch must be normalized apply_patch text:\n%s", dp)
	}

	// 坏补丁：置空、finding 保留（诚实降级，不编造补丁）
	bad := mapSandboxFindings("t-fix", dir, []sandbox.Finding{{
		Title: "SQL 注入", Severity: "SEVERITY_CRITICAL",
		FilePath: "vuln_fix_demo.py", StartLine: 6,
		FixSuggestion: "## 说明",
		DiffPatch:     strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( bad )", 1),
	}})
	if len(bad) != 1 {
		t.Fatalf("finding must be kept on patch rejection, got %d", len(bad))
	}
	if bad[0].GetDiffPatch() != "" {
		t.Fatalf("rejected patch must be dropped, got:\n%s", bad[0].GetDiffPatch())
	}
	if bad[0].GetAiFixSuggestion() == "" {
		t.Fatal("markdown suggestion is independent of patch validity")
	}
}

func TestBuildFixSuggestion(t *testing.T) {
	// 沙箱来源（有真实建议+已校验补丁）→ markdown + diff_patch（ADR-176 4c 债出口）
	f := &pb.UnifiedFinding{
		FindingId: "t-1-sbx-1", Title: "SQL 注入", SourceRuleId: "dsh-headless",
		AiFixSuggestion: "## 修复\n参数化", AiConfidence: 0.95,
		DiffPatch: "*** Begin Patch\n*** End Patch",
		Location:  &pb.LocationInfo{FilePath: "a.py", StartLine: 6},
	}
	fs := buildFixSuggestion(f)
	if fs.GetDiffPatch() != f.GetDiffPatch() || fs.GetCodeFix() != f.GetDiffPatch() {
		t.Fatalf("diff_patch/code_fix must carry validated patch: %q / %q", fs.GetDiffPatch(), fs.GetCodeFix())
	}
	if fs.GetDescription() != f.GetAiFixSuggestion() {
		t.Fatalf("description must be LLM markdown, got %q", fs.GetDescription())
	}
	if fs.GetConfidence() != 0.95 {
		t.Fatalf("confidence must carry model confidence, got %v", fs.GetConfidence())
	}

	// RuleScan 降级/无建议 → MANUAL_REVIEW_REQUIRED 占位，绝无编造补丁
	g := &pb.UnifiedFinding{
		FindingId: "t-1-rs-1", Title: "疑似注入", SourceRuleId: "rulescan-fallback:RULE-SQL-001",
		Location: &pb.LocationInfo{FilePath: "b.py", StartLine: 3},
	}
	gs := buildFixSuggestion(g)
	if gs.GetDiffPatch() != "" || gs.GetCodeFix() != "" {
		t.Fatalf("fallback finding must not carry any patch, got %q", gs.GetDiffPatch())
	}
	if !strings.HasPrefix(gs.GetDescription(), "MANUAL_REVIEW_REQUIRED") {
		t.Fatalf("fallback must keep honest placeholder, got %q", gs.GetDescription())
	}
}

// ---- ADR-183 补遗②：解析器容错加固（Cline normalizePatchInput/peek 语义） ----

func TestNormalizeDiffPatch_InputHardening(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	// 基准产出（与精确全文断言用例同源）
	want, err := NormalizeDiffPatch(vulnFixDemoPatch, dir)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// ① CRLF 行尾：所有行 \n→\r\n 后仍须归一出同一补丁
	crlf := strings.ReplaceAll(vulnFixDemoPatch, "\n", "\r\n")
	if got, err := NormalizeDiffPatch(crlf, dir); err != nil || got != want {
		t.Fatalf("CRLF patch must normalize identically: err=%v", err)
	}
	// ② 补丁前后带解释文字（双 sentinel 齐→切取其间）
	preamble := "好的，以下是修复补丁：\n" + vulnFixDemoPatch + "\n以上补丁将拼接改为参数化查询。"
	if got, err := NormalizeDiffPatch(preamble, dir); err != nil || got != want {
		t.Fatalf("preamble-wrapped patch must normalize identically: err=%v", err)
	}
	// ③ 双 sentinel 全缺 + 壳行包裹（%%bash/EOF）→ 剥壳补 sentinel
	shelled := "%%bash\napply_patch <<\"EOF\"\n" +
		strings.TrimSuffix(strings.TrimPrefix(vulnFixDemoPatch, "*** Begin Patch\n"), "\n*** End Patch") +
		"\nEOF"
	if got, err := NormalizeDiffPatch(shelled, dir); err != nil || got != want {
		t.Fatalf("shell-wrapped sentinel-less patch must normalize identically: err=%v", err)
	}
	// ④ 仅一侧 sentinel → 硬错误（Cline incomplete sentinels 同款）
	if _, err := NormalizeDiffPatch(strings.TrimSuffix(vulnFixDemoPatch, "*** End Patch"), dir); err == nil {
		t.Fatal("single sentinel must be a hard error")
	}
	// ⑤（裸行上下文的 peek 容错单列 TestNormalizeDiffPatch_BareContextLineTolerated；
	// 此处保留一条锚点+删除行连续的正确单删行用例作基线对照）
	singleDel := "*** Begin Patch\n*** Update File: vuln_fix_demo.py\n@@     query = \"SELECT * FROM users WHERE id = '\" + user_id + \"'\"\n" +
		"-    cursor.execute(query)\n+    cursor.execute(query, (user_id,))\n*** End Patch"
	if _, err := NormalizeDiffPatch(singleDel, dir); err != nil {
		t.Fatalf("single-del sanity: %v", err)
	}
	// ⑥ 未识别 *** 指令行 → fail fast（不静默吞）
	badDirective := strings.Replace(vulnFixDemoPatch, "+    # 参数化查询：用户输入不进入 SQL 文本\n", "*** Special Directive: x\n", 1)
	if _, err := NormalizeDiffPatch(badDirective, dir); err == nil {
		t.Fatal("unknown *** directive must fail fast")
	}
}

func TestNormalizeDiffPatch_BareContextLineTolerated(t *testing.T) {
	// Cline peek：上下文行漏写前导空格（缩进行首恰为内容）→ 自动补空格当上下文，不丢弃
	dir := writeDemoWs(t, vulnFixDemo)
	// 锚点行裸写（无 @@ 也可裸行开段）：@@ 后跟的上下文行本例由锚点承担；
	// 此处构造：@@ 锚点 + 裸写上下文行（def 行，无前导空格）+ 删除/新增
	p := "*** Begin Patch\n*** Update File: vuln_fix_demo.py\n" +
		"@@     cursor = conn.cursor()\n" +
		"def get_user(conn, user_id):\n" + // 裸行——peek 视作上下文（该行在锚点行之前的真实位置不成立→将锚定失败？）
		"-    cursor.execute(query)\n" +
		"+    cursor.execute(query, (user_id,))\n" +
		"*** End Patch"
	_, err := NormalizeDiffPatch(p, dir)
	// 裸行被吸收为上下文（不报"malformed"），但顺序与文件不符会被内容锚定正确拒绝——
	// 两种结局都合法；断言：绝不因裸行本身报语法错
	if err != nil && strings.Contains(err.Error(), "malformed") {
		t.Fatalf("bare context line must not be a syntax error: %v", err)
	}
}

func TestNormalizeDiffPatch_EscapedQuoteCanonicalize(t *testing.T) {
	// Cline canonicalize 反转义：\" → " （比较侧）
	dir := writeDemoWs(t, vulnFixDemo)
	p := strings.Replace(vulnFixDemoPatch,
		`-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"`,
		`-    query = \"SELECT * FROM users WHERE id = '\" + user_id + \"'\"`, 1)
	if _, err := NormalizeDiffPatch(p, dir); err != nil {
		t.Fatalf("escaped quotes must canonicalize equal: %v", err)
	}
}

func TestNormalizeDiffPatch_FailureDetailRichness(t *testing.T) {
	// Cline formatSkippedHunkFailure 语义：失败反馈含相似度数值 + ≤200 字符上下文预览
	// （这是再生成回合模型自纠的输入质量）
	dir := writeDemoWs(t, vulnFixDemo)
	bad := strings.Replace(vulnFixDemoPatch,
		"-    cursor.execute(query)", "-    cursor.execute( query )", 1)
	_, err := NormalizeDiffPatch(bad, dir)
	if err == nil {
		t.Fatal("must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "similarity") || !strings.Contains(msg, "Context:") {
		t.Fatalf("failure detail must carry similarity + context preview: %s", msg)
	}
	if !strings.Contains(msg, "0.") { // 相似度数值（本例接近但 <1）
		t.Fatalf("similarity value missing: %s", msg)
	}
}

// ---- ADR-183 补遗②：失败反馈再生成闭环 ----

func TestRetryFailedPatches_SelfCorrectionCycle(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	badPatch := strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( bad )", 1)
	findings := []sandbox.Finding{
		{Title: "SQL 注入", FilePath: "vuln_fix_demo.py", StartLine: 6, DiffPatch: badPatch},
		{Title: "干净项", FilePath: "vuln_fix_demo.py", StartLine: 4, DiffPatch: vulnFixDemoPatch},
	}
	var assignment string
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		assignment = a
		return []sandbox.PatchFix{{Index: 0, DiffPatch: vulnFixDemoPatch}}, nil
	}
	got := retryFailedPatches(context.Background(), "t-retry", dir, findings, func(level, msg string) {}, round)
	// 反馈任务指令包含失败详情与格式规范
	for _, want := range []string{"similarity", "Context:", "index", "bad_diff_patch", "*** Update File:"} {
		if !strings.Contains(assignment, want) {
			t.Fatalf("retry assignment missing %q:\n%s", want, assignment)
		}
	}
	// 复验通过的原位替换为规范化补丁
	if got[0].DiffPatch == badPatch || !strings.HasPrefix(got[0].DiffPatch, "*** Begin Patch") {
		t.Fatalf("failed patch must be replaced by normalized one:\n%s", got[0].DiffPatch)
	}
	if got[1].DiffPatch != vulnFixDemoPatch {
		t.Fatal("clean finding must be untouched (no retry round item)")
	}
}

func TestRetryFailedPatches_StillFailsAfterRetry(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	badPatch := strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( bad )", 1)
	findings := []sandbox.Finding{{Title: "SQL 注入", FilePath: "vuln_fix_demo.py", StartLine: 6, DiffPatch: badPatch}}
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) {
		return []sandbox.PatchFix{{Index: 0, DiffPatch: badPatch}}, nil
	}
	got := retryFailedPatches(context.Background(), "t-retry", dir, findings, func(level, msg string) {}, round)
	if got[0].DiffPatch != badPatch {
		t.Fatal("still-bad patch must stay raw (mapping drops it later) — 不豁免校验")
	}
}

func TestRetryFailedPatches_RoundFailureKeepsOriginal(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	badPatch := strings.Replace(vulnFixDemoPatch, "-    cursor.execute(query)", "-    cursor.execute( bad )", 1)
	findings := []sandbox.Finding{{Title: "SQL 注入", FilePath: "vuln_fix_demo.py", StartLine: 6, DiffPatch: badPatch}}
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) { return nil, fmt.Errorf("sandbox down") }
	got := retryFailedPatches(context.Background(), "t-retry", dir, findings, nil, round)
	if got[0].DiffPatch != badPatch {
		t.Fatal("round failure must keep original raw (dropped later by mapping)")
	}
}

func TestRetryFailedPatches_NoFailuresNoRound(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	findings := []sandbox.Finding{
		{Title: "干净", FilePath: "vuln_fix_demo.py", DiffPatch: vulnFixDemoPatch},
		{Title: "未产补丁", FilePath: "vuln_fix_demo.py"}, // raw 为空：不参与（模型主动不给≠写坏）
	}
	called := false
	round := func(ctx context.Context, a string) ([]sandbox.PatchFix, error) { called = true; return nil, nil }
	retryFailedPatches(context.Background(), "t-retry", dir, findings, nil, round)
	if called {
		t.Fatal("no failures → no retry round (零开销)")
	}
}

// ---- R34 回归锁（实证）：沙箱挂载视角路径容错 ----
// 模型在沙箱内看到的项目根是 /sandbox/project，产出补丁把段路径写成挂载视角
// （"project/x" / "sandbox/project/x"），校验端按工作区根解析 → 13/13 补丁全拒。
// 容错=原路径不存在且剥挂载前缀后存在时改写段路径；产出补丁同步改写。

func TestNormalizeDiffPatch_SandboxMountPrefixRewrite(t *testing.T) {
	ws := t.TempDir()
	nested := filepath.Join(ws, "librawspeed", "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "void f() {\n    bad();\n}\n"
	if err := os.WriteFile(filepath.Join(nested, "x.cpp"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(path string) string {
		return "*** Begin Patch\n*** Update File: " + path + "\n void f() {\n-    bad();\n+    good();\n*** End Patch"
	}
	// "project/" 挂载前缀：校验通过，产出路径改写为工作区相对形态
	out, err := NormalizeDiffPatch(mk("project/librawspeed/src/x.cpp"), ws)
	if err != nil {
		t.Fatalf("sandbox-mount-relative path must resolve: %v", err)
	}
	if !strings.Contains(out, "*** Update File: librawspeed/src/x.cpp\n") {
		t.Fatalf("rewritten patch must carry workspace-relative path:\n%s", out)
	}
	// 双重前缀形态（绝对路径 /sandbox/project/x 经段清洗后）
	if _, err := NormalizeDiffPatch(mk("sandbox/project/librawspeed/src/x.cpp"), ws); err != nil {
		t.Fatalf("double-prefixed form must resolve: %v", err)
	}
	// 产出补丁（已改写路径）可再规范化且路径不再改写（幂等）
	if _, err := NormalizeDiffPatch(out, ws); err != nil {
		t.Fatalf("rewritten output must re-normalize: %v", err)
	}
}

func TestNormalizeDiffPatch_GenuineProjectDirNotStripped(t *testing.T) {
	// 工作区真有顶层 project/ 目录：原路径存在 → 不剥（防误伤合法仓结构）
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "project", "real.py"), []byte("x = 1\ny = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n*** Update File: project/real.py\n x = 1\n-y = 2\n+y = 3\n*** End Patch"
	out, err := NormalizeDiffPatch(p, ws)
	if err != nil {
		t.Fatalf("genuine project/ dir must keep original path: %v", err)
	}
	if !strings.Contains(out, "*** Update File: project/real.py\n") {
		t.Fatalf("original path must be preserved:\n%s", out)
	}
}

func TestNormalizeDiffPatch_AddFileWithMountPrefix(t *testing.T) {
	// Add File 目标必须不存在，容错以"父目录存在"为准：剥前缀后父目录在 → 改写
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n*** Add File: project/pkg/new.py\n+print('n')\n*** End Patch"
	out, err := NormalizeDiffPatch(p, ws)
	if err != nil {
		t.Fatalf("add with mount prefix must resolve via parent dir: %v", err)
	}
	if !strings.Contains(out, "*** Add File: pkg/new.py\n") {
		t.Fatalf("add path must be rewritten:\n%s", out)
	}
	// 两边父目录都不存在 → 原样保留，错误携带原路径（失败反馈保真）
	_, err = NormalizeDiffPatch("*** Begin Patch\n*** Update File: project/no/such.py\n ctx\n-a\n+b\n*** End Patch", ws)
	if err == nil || !strings.Contains(err.Error(), "project/no/such.py") {
		t.Fatalf("unresolvable path must error with original path, got: %v", err)
	}
}

// jsonQuote — 测试辅助：Go 字符串→JSON 字符串字面量。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestNormalizeDiffPatch_BareSectionMarker — GLM function-calling 实测形态（evidence
// 15_glm_schema_test_args.json）：裸 "@@" 无锚内容 + 显式上下文行自锚定 + 多行上下文。
func TestNormalizeDiffPatch_BareSectionMarker(t *testing.T) {
	dir := writeDemoWs(t, vulnFixDemo)
	p := "*** Begin Patch\n*** Update File: vuln_fix_demo.py\n" +
		"@@\n" +
		" def get_user(conn, user_id):\n" +
		"     cursor = conn.cursor()\n" +
		`-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"` + "\n" +
		"-    cursor.execute(query)\n" +
		`+    cursor.execute("SELECT * FROM users WHERE id = ?", (user_id,))` + "\n" +
		"     # 执行查询并返回结果\n" +
		"     return cursor.fetchone()\n" +
		"*** End Patch"
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("bare @@ patch rejected: %v", err)
	}
	if !strings.Contains(out, "@@ def get_user(conn, user_id):") {
		t.Fatalf("normalized head context must become the anchor line:\n%s", out)
	}
	if !strings.Contains(out, "+    # 参数化查询") && !strings.Contains(out, `+    cursor.execute("SELECT * FROM users WHERE id = ?", (user_id,))`) {
		// 本例新增行为替换式；断言替换行存在即可
		t.Fatalf("addition line lost:\n%s", out)
	}
	if strings.Contains(out, "-    return cursor.fetchone()") {
		t.Fatalf("return line must stay context, not deletion:\n%s", out)
	}
}

// TestNormalizeDiffPatch_AnchorIndentTolerance — 首行（@@ 锚点）缩进漂移容错
// （实证回归锁）：模型补丁的 @@ 锚点丢了 4 空格缩进、其余行全部逐字
//（similarity 0.97），此前被 fuzz=0 整补丁拒绝并触发一整轮再生成沙箱。
// 修复后：锚点行按 trim+canonicalize 定位真实文件行锚定，@@ 行以工作区逐字行
// 回写——产出补丁对消费端仍 fuzz=0，且规范化幂等。
func TestNormalizeDiffPatch_AnchorIndentTolerance(t *testing.T) {
	// 真实工作区：本任务上传的 vuln_demo.py（374 字节项目）
	const vulnDemo = `import sqlite3

def get_user(user_id):
    conn = sqlite3.connect("app.db")
    cursor = conn.cursor()
    # SQL injection: string concatenation
    query = "SELECT * FROM users WHERE id = " + user_id
    cursor.execute(query)
    return cursor.fetchone()
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vuln_demo.py"), []byte(vulnDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模型实际提交的补丁原文（ai.log 取证）：@@ cursor = conn.cursor() 无缩进
	raw := "*** Begin Patch\n" +
		"*** Update File: vuln_demo.py\n" +
		"@@ cursor = conn.cursor()\n" +
		"-    # SQL injection: string concatenation\n" +
		`-    query = "SELECT * FROM users WHERE id = " + user_id` + "\n" +
		"-    cursor.execute(query)\n" +
		"+    # Parameterized query: placeholder is bound by the driver, preventing SQL injection\n" +
		`+    query = "SELECT * FROM users WHERE id = ?"` + "\n" +
		"+    cursor.execute(query, (user_id,))\n" +
		"*** End Patch"
	out, err := NormalizeDiffPatch(raw, dir)
	if err != nil {
		t.Fatalf("anchor indent drift must be tolerated: %v", err)
	}
	// @@ 行以工作区逐字行（含 4 空格缩进）回写——消费端 @@ 精确层 fuzz=0
	want := "*** Begin Patch\n" +
		"*** Update File: vuln_demo.py\n" +
		"@@     cursor = conn.cursor()\n" +
		"-    # SQL injection: string concatenation\n" +
		`-    query = "SELECT * FROM users WHERE id = " + user_id` + "\n" +
		"-    cursor.execute(query)\n" +
		"+    # Parameterized query: placeholder is bound by the driver, preventing SQL injection\n" +
		`+    query = "SELECT * FROM users WHERE id = ?"` + "\n" +
		"+    cursor.execute(query, (user_id,))\n" +
		"*** End Patch"
	if out != want {
		t.Fatalf("normalized patch mismatch:\n--got--\n%s\n--want--\n%s", out, want)
	}
	// 幂等：规范化输出再规范化不变（@@ 行已逐字，精确层直通）
	again, err := NormalizeDiffPatch(out, dir)
	if err != nil || again != out {
		t.Fatalf("must be idempotent after tolerance: err=%v", err)
	}
}

// 对照锁：非首行（普通上下文/删除行）的缩进漂移仍被拒绝——那是真改写风险，
// 容错只限 @@ 锚点行（模型转写 "@@ <行>" 时前导空白不可见且易丢的特定失效模式）。
func TestNormalizeDiffPatch_MiddleLineIndentDriftStillRejected(t *testing.T) {
	const vulnDemo = `import sqlite3

def get_user(user_id):
    conn = sqlite3.connect("app.db")
    cursor = conn.cursor()
    # SQL injection: string concatenation
    query = "SELECT * FROM users WHERE id = " + user_id
    cursor.execute(query)
    return cursor.fetchone()
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vuln_demo.py"), []byte(vulnDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	// 删除行 #2 丢缩进（"-    query..." 写成 "-    query" 但首行锚点逐字正确→精确层失败
	// 且其余行不齐→容错层也必须拒绝）
	raw := "*** Begin Patch\n" +
		"*** Update File: vuln_demo.py\n" +
		"@@     cursor = conn.cursor()\n" +
		"-    # SQL injection: string concatenation\n" +
		`-    query = "SELECT * FROM users WHERE id = " + user_id` + "\n" +
		"-  cursor.execute(query)\n" + // 缩进漂移（6 空格 ≠ 4 空格）
		"+    # fixed\n" +
		"*** End Patch"
	if _, err := NormalizeDiffPatch(raw, dir); err == nil {
		t.Fatal("middle-line indent drift must stay rejected")
	}
}

// ---- R37 回归锁（实证）：@@ 锚点对齐 Cline 摄入语义 ----
// Cline apply-patch-parser：@@ defStr 是寻位指令（canonTrim/trim 容错匹配文件行），
// 不物化为 hunk 内容；锚定只依赖显式上下文/删除行。此前引擎把 defStr 物化为首条
// 上下文行 + 1 空格去重，模型三种自然书写形态（丢缩进双写/夹新增双写/锚点幻觉）
// 整补丁被拒。

// r37ws — 真实工作区（7 行 app.py）。
const r37ws = `import sqlite3
def get_user(uid):
    conn = sqlite3.connect("app.db")
    cur = conn.cursor()
    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)  # SQL 注入
    return cur.fetchone()
API_TOKEN = "hunter2-hardcoded-secret"  # 硬编码凭据
`

// 形态一（实证 2/2）：锚点丢缩进 + 显式重复同一行。
func TestNormalizeDiffPatch_AnchorDoubleWriteUnindented(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(r37ws), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n" +
		"*** Update File: app.py\n" +
		"@@ cur = conn.cursor()\n" + // 锚点裸写（丢 4 格缩进）
		"     cur = conn.cursor()\n" + // 显式上下文重复（含缩进）
		"-    cur.execute(\"SELECT * FROM users WHERE id = '%s'\" % uid)  # SQL 注入\n" +
		"+    cur.execute(\"SELECT * FROM users WHERE id = ?\", (uid,))\n" +
		"*** End Patch"
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("double-written unindented anchor must resolve (Cline defStr semantics): %v", err)
	}
	// 重建补丁 @@ 承载真实文件行（含缩进）——消费端 fuzz=0 契约不变
	if !strings.Contains(out, "@@     cur = conn.cursor()\n") {
		t.Fatalf("rebuilt @@ must carry verbatim file line:\n%s", out)
	}
}

// 形态一·EOF 档（第 2 项）：同形态 + *** End of File。
func TestNormalizeDiffPatch_AnchorDoubleWriteUnindentedEof(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(r37ws), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n" +
		"*** Update File: app.py\n" +
		"@@ return cur.fetchone()\n" +
		"     return cur.fetchone()\n" +
		"-API_TOKEN = \"hunter2-hardcoded-secret\"  # 硬编码凭据\n" +
		"+API_TOKEN = os.environ[\"API_TOKEN\"]\n" +
		"*** End of File\n" +
		"*** End Patch"
	if _, err := NormalizeDiffPatch(p, dir); err != nil {
		t.Fatalf("double-write + EOF hunk must resolve: %v", err)
	}
}

// 形态二（实证）：锚点 + 中间夹 +新增 + 显式重复（插入式双写）。
func TestNormalizeDiffPatch_AnchorDoubleWriteWithAddition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(r37ws), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n" +
		"*** Update File: app.py\n" +
		"@@ import sqlite3\n" +
		"+import os\n" +
		" import sqlite3\n" +
		"@@     return cur.fetchone()\n" +
		"-API_TOKEN = \"hunter2-hardcoded-secret\"  # 硬编码凭据\n" +
		"+API_TOKEN = os.environ[\"API_TOKEN\"]\n" +
		"*** End Patch"
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("insertion-style double-write must resolve: %v", err)
	}
	if !strings.Contains(out, "+import os\n") {
		t.Fatalf("insertion must be preserved:\n%s", out)
	}
}

// 形态三：锚点幻觉（defStr 在文件中不存在）但显式上下文自足 → 内容锚定兜底通过。
func TestNormalizeDiffPatch_AnchorHallucinatedHintIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(r37ws), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n" +
		"*** Update File: app.py\n" +
		"@@ def process(uid):\n" + // 文件中不存在此行
		"-    cur.execute(\"SELECT * FROM users WHERE id = '%s'\" % uid)  # SQL 注入\n" +
		"+    cur.execute(\"SELECT * FROM users WHERE id = ?\", (uid,))\n" +
		"*** End Patch"
	if _, err := NormalizeDiffPatch(p, dir); err != nil {
		t.Fatalf("hallucinated defStr must degrade to content anchoring: %v", err)
	}
}

// 形态三反面：纯新增 hunk 无 defStr（位置不可锚定）仍拒绝；defStr 可用则允许。
func TestNormalizeDiffPatch_PureAdditionAnchoring(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(r37ws), 0o644); err != nil {
		t.Fatal(err)
	}
	// 裸 @@ + 纯新增：无任何锚定依据 → 拒绝（位置歧义，不编造）
	p := "*** Begin Patch\n*** Update File: app.py\n@@\n+import os\n*** End Patch"
	if _, err := NormalizeDiffPatch(p, dir); err == nil {
		t.Fatal("bare-@@ pure addition must stay rejected (no anchor)")
	}
	// defStr 给出位置 → 插入锚定到真实行
	p2 := "*** Begin Patch\n*** Update File: app.py\n@@ import sqlite3\n+import os\n*** End Patch"
	out, err := NormalizeDiffPatch(p2, dir)
	if err != nil {
		t.Fatalf("defStr-anchored pure addition must resolve: %v", err)
	}
	if !strings.Contains(out, "@@ import sqlite3\n+import os\n") {
		t.Fatalf("insertion anchored at defStr line:\n%s", out)
	}
}

// 形态四（实证 2/4）：文件顶（idx==0）带删除行——@@ 即被删首行，
// 重建 @@ 承载首条变更行本身（消费端插件经全文回扫兜底层应用）。
func TestNormalizeDiffPatch_AnchorAtFileTopDeletion(t *testing.T) {
	dir := t.TempDir()
	ws := "import os\n\ndef rm(p):\n    os.system(\"rm -rf \" + p)\n"
	if err := os.WriteFile(filepath.Join(dir, "utils.py"), []byte(ws), 0o644); err != nil {
		t.Fatal(err)
	}
	p := "*** Begin Patch\n" +
		"*** Update File: utils.py\n" +
		"@@ import os\n" +
		"-import os\n" +
		"+import os\n" +
		"+import subprocess\n" +
		"@@ def rm(p):\n" +
		"-    os.system(\"rm -rf \" + p)\n" +
		"+    subprocess.run([\"rm\", \"-rf\", \"--\", p], check=True)\n" +
		"*** End Patch"
	out, err := NormalizeDiffPatch(p, dir)
	if err != nil {
		t.Fatalf("file-top deletion with @@ as deleted line must resolve: %v", err)
	}
	if !strings.Contains(out, "@@ import os\n") {
		t.Fatalf("rebuilt @@ must carry the first changed line:\n%s", out)
	}
	if !strings.Contains(out, "-import os\n+import os\n+import subprocess\n") {
		t.Fatalf("replacement semantics must be preserved:\n%s", out)
	}
}
