// project_profile_test.go — prompt 接地一期（docs/designs/ai-prompt-grounding-2026-10.md §3.3）:
// 画像渲染（空项目退化/确定性/封顶截断/清单根探测/入口候选）+ 组装字节兼容 + 遍历集成。
package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// walkAndProbe — 生产路径同款组装：遍历回调（仅代码文件会到达）+ 根探测清单。
func walkAndProbe(t *testing.T, root string) *profileCollector {
	t.Helper()
	c := newProfileCollector()
	if err := filepathWalkLite(root, func(p string, lines int32, lang string) bool {
		c.observeCodeFile(p, lines, lang)
		return true
	}); err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	c.probeManifests(root)
	return c
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		abs := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(abs), 0o755)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("写夹具失败 %s: %v", p, err)
		}
	}
	return dir
}

func TestProjectProfile_EmptyProjectRendersEmpty(t *testing.T) {
	c := newProfileCollector()
	if got := c.render(); got != "" {
		t.Fatalf("空项目应渲染空串（组装面退化路径），得到 %q", got)
	}
	if a := withProfile("任务指令", c.render()); a != "任务指令" {
		t.Fatalf("空画像必须字节兼容原 assignment，得到 %q", a)
	}
}

func TestProjectProfile_WalkRenderDeterministicAndBounded(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"app.py":       "a\nb\nc\n",
		"lib/util.py":  "a\n",
		"main.go":      "package main\n",
		"index.js":     "x\n",
		"go.mod":       "module x\n",
		"package.json": "{}\n",
	})
	got := walkAndProbe(t, dir).render()
	for _, want := range []string{
		"## 项目画像（自动盘点，供聚焦参考；与实际不符时以沙箱目录实测为准）",
		"python×2",    // 文件数降序居首
		"go×1",        // 同数按名升序: go < javascript
		"（共 4 文件/",    // 代码文件 4（清单文件不进 walkFn 回调——codeExt 过滤,设计即此）
		"go.mod(→Go)", // 清单根探测（无代码扩展名,walkFn 不回调）
		"package.json(→Node.js)",
		"入口候选: app.py, index.js, main.go", // walk 字典序
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("画像缺段 %q\n实际:\n%s", want, got)
		}
	}
	// 确定性:同输入两次渲染逐字节相等
	c := walkAndProbe(t, dir)
	if got != c.render() {
		t.Fatalf("两次渲染不一致（排序非确定）:\n%s\n---\n%s", got, c.render())
	}
}

func TestProjectProfile_LangCapAnnotations(t *testing.T) {
	c := newProfileCollector()
	for _, l := range []string{"go", "python", "javascript", "typescript", "java", "c", "cpp"} {
		c.observeCodeFile("/p/f"+l+extOf(l), 1, l)
	}
	got := c.render()
	if !strings.Contains(got, "另 2 种语言未列出") {
		t.Fatalf("超限语言数未如实标注:\n%s", got)
	}
	if strings.Count(got, "×1") > profileMaxLangs {
		t.Fatalf("语言列表未封顶 %d:\n%s", profileMaxLangs, got)
	}
}

func extOf(lang string) string {
	switch lang {
	case "go":
		return ".go"
	case "python":
		return ".py"
	case "javascript":
		return ".js"
	case "typescript":
		return ".ts"
	case "java":
		return ".java"
	default:
		return "." + lang
	}
}

func TestProjectProfile_ManifestCapSortedAndCsproj(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"App.csproj":       "<Project/>\n",
		"Cargo.toml":       "[package]\n",
		"composer.json":    "{}\n",
		"go.mod":           "module x\n",
		"package.json":     "{}\n",
		"pom.xml":          "<project/>\n",
		"requirements.txt": "flask\n",
	})
	c := newProfileCollector()
	c.probeManifests(dir)
	if c.manifestTotal != 7 {
		t.Fatalf("清单命中总数应=7，得到 %d", c.manifestTotal)
	}
	if len(c.manifests) != profileMaxManifests {
		t.Fatalf("清单保留数应封顶 %d，得到 %d", profileMaxManifests, len(c.manifests))
	}
	got := c.render()
	if !strings.Contains(got, "另 3 份清单未列出") {
		t.Fatalf("超限清单未如实标注:\n%s", got)
	}
	// 字典序截断:App.csproj(A) 居首保留,csproj 后缀特例识别为 C#
	if !strings.Contains(got, "App.csproj(→C#)") {
		t.Fatalf("csproj 后缀匹配缺失:\n%s", got)
	}
	if !strings.Contains(got, "Cargo.toml(→Rust)") {
		t.Fatalf("字典序截断语义不符:\n%s", got)
	}
}

func TestProjectProfile_EntryCap(t *testing.T) {
	c := newProfileCollector()
	for _, e := range []string{"main.py", "app.py", "manage.py", "index.js", "app.js", "server.js", "index.ts", "app.ts", "main.go", "server.ts"} {
		c.observeCodeFile("/p/src/"+e, 1, "")
	}
	if len(c.entries) != profileMaxEntries {
		t.Fatalf("入口候选应封顶 %d，得到 %d", profileMaxEntries, len(c.entries))
	}
}

func TestWithProfile_ComposesAndPreservesInstruction(t *testing.T) {
	base := sandboxAssignmentModeA()
	got := withProfile(base, "- 语言分布: python×1（共 1 文件/1 行）")
	if !strings.Contains(got, base) {
		t.Fatalf("组合必须保留原指令全文（Contains 契约,与 sandbox_incremental_test 同判据）")
	}
	if !strings.Contains(got, "语言分布") {
		t.Fatalf("组合必须包含画像段")
	}
	if !strings.HasSuffix(got, "（共 1 文件/1 行）") {
		t.Fatalf("画像段应在尾部:\n%s", got)
	}
}
