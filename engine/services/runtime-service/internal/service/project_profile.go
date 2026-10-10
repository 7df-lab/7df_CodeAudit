// project_profile.go — 检测回合的项目画像。
// 设计档：主仓库 docs/designs/ai-prompt-grounding-2026-10.md §3。
// 数据源两路：①Code Analyst 既有遍历的 per-file 回调（walkFn 只回调 codeExt 代码文件，
// 语言分布/入口候选由此来）；②依赖清单对项目根定点 stat 探测（清单文件无代码扩展名，
// walkFn 不回调；根探测约 10 次 stat，零新增遍历；子目录清单不在探测范围=已知边界）。
// 渲染有界（语言≤5/清单≤4/入口≤8），与 buildTurnPrompt 的"prompt 体量与项目大小无关"
// 原则同源。画像只是先验：段首免责声明钉死"模型以沙箱目录实测为准"，防止盘点过期误导。
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 画像渲染封顶（设计档 §3.1：列表全部封顶，超出截断并标注总数）。
const (
	profileMaxLangs     = 5
	profileMaxManifests = 4
	profileMaxEntries   = 8
)

// profileManifestKinds — 依赖清单文件名 → 栈推断提示（项目根定点探测）。
var profileManifestKinds = map[string]string{
	"package.json":     "Node.js",
	"requirements.txt": "Python",
	"pyproject.toml":   "Python",
	"Pipfile":          "Python",
	"go.mod":           "Go",
	"pom.xml":          "Java(Maven)",
	"build.gradle":     "Java(Gradle)",
	"Cargo.toml":       "Rust",
	"composer.json":    "PHP",
}

// profileEntryNames — 常见入口文件（代码遍历回调内 basename 匹配；命中即候选，截断封顶）。
var profileEntryNames = map[string]bool{
	"main.go": true, "main.py": true, "app.py": true, "manage.py": true,
	"app.js": true, "app.ts": true, "index.js": true, "index.ts": true,
	"server.js": true, "server.ts": true, "main.java": true, "Application.java": true,
}

// profileCollector — 遍历回调+根探测期间顺带收集画像素材；不保留全量文件表（体量无关原则）。
type profileCollector struct {
	langs         map[string]int32 // lang → 文件数
	langLines     map[string]int64 // lang → 行数
	files         int32
	lines         int64
	manifests     []string // 探测命中（字典序，确定性），封顶截断
	manifestTotal int      // 清单命中总数（含截断未保留者，供"另 N 份"如实标注）
	entries       []string
}

func newProfileCollector() *profileCollector {
	return &profileCollector{
		langs:     map[string]int32{},
		langLines: map[string]int64{},
	}
}

// observeCodeFile — walkFn 回调内调用（仅代码文件会到达）：语言分布+入口候选。
func (c *profileCollector) observeCodeFile(path string, lines int32, lang string) {
	base := filepath.Base(path)
	c.files++
	c.lines += int64(lines)
	if lang != "" {
		c.langs[lang]++
		c.langLines[lang] += int64(lines)
	}
	if profileEntryNames[base] && len(c.entries) < profileMaxEntries {
		c.entries = append(c.entries, base)
	}
}

// probeManifests — 项目根定点探测依赖清单（stat 级，零遍历）；目录不存在=静默跳过
// （调用方遍历已失败时画像整体为空，此处不重复报错）。
func (c *profileCollector) probeManifests(root string) {
	if root == "" {
		return
	}
	names := make([]string, 0, len(profileManifestKinds)+1)
	for name := range profileManifestKinds {
		names = append(names, filepath.Join(root, name))
	}
	if cs, err := filepath.Glob(filepath.Join(root, "*.csproj")); err == nil {
		names = append(names, cs...) // 绝对路径，识别分支按后缀匹配
	}
	sort.Strings(names) // 确定性：字典序截断
	for _, p := range names {
		base := filepath.Base(p)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		c.manifestTotal++
		if len(c.manifests) >= profileMaxManifests {
			continue
		}
		if kind, ok := profileManifestKinds[base]; ok {
			c.manifests = append(c.manifests, fmt.Sprintf("%s(→%s)", base, kind))
			continue
		}
		if strings.HasSuffix(base, ".csproj") {
			c.manifests = append(c.manifests, fmt.Sprintf("%s(→C#)", base))
		}
	}
}

// render — 有界画像段；空项目（0 文件且 0 清单）返回空串（组装面退化为现行为，字节兼容）。
func (c *profileCollector) render() string {
	if c.files == 0 && c.manifestTotal == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 项目画像（自动盘点，供聚焦参考；与实际不符时以沙箱目录实测为准）\n")
	// 语言分布：文件数降序、同数按名升序（确定性）；超出封顶标注总种数
	type langStat struct {
		name  string
		count int32
	}
	stats := make([]langStat, 0, len(c.langs))
	for name, count := range c.langs {
		stats = append(stats, langStat{name, count})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].count != stats[j].count {
			return stats[i].count > stats[j].count
		}
		return stats[i].name < stats[j].name
	})
	langParts := make([]string, 0, len(stats))
	for i, s := range stats {
		if i >= profileMaxLangs {
			break
		}
		langParts = append(langParts, fmt.Sprintf("%s×%d", s.name, s.count))
	}
	langLine := fmt.Sprintf("- 语言分布: %s（共 %d 文件/%d 行", strings.Join(langParts, ", "), c.files, c.lines)
	if len(stats) > profileMaxLangs {
		langLine += fmt.Sprintf("，另 %d 种语言未列出", len(stats)-profileMaxLangs)
	}
	langLine += "）"
	b.WriteString(langLine + "\n")
	if len(c.manifests) > 0 {
		tail := ""
		if c.manifestTotal > profileMaxManifests {
			tail = fmt.Sprintf("；另 %d 份清单未列出", c.manifestTotal-profileMaxManifests)
		}
		b.WriteString("- 依赖清单: " + strings.Join(c.manifests, "; ") + tail + "\n")
	}
	if len(c.entries) > 0 {
		b.WriteString("- 入口候选: " + strings.Join(c.entries, ", ") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// withProfile — 画像段拼接到任务指令尾部；空画像=原样返回（字节兼容路径）。
func withProfile(assignment, profile string) string {
	if profile == "" {
		return assignment
	}
	return assignment + "\n\n" + profile
}
