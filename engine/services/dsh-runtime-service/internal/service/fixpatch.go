// fixpatch — ADR-183: diff_patch 服务端校验与规范化。
//
// 沙箱 DSH 产出的 apply_patch 补丁文本经本模块校验重建后才允许进入 UnifiedFinding.diff_patch：
//   - Update File 段按"顺序游标 + first-hit + NFC canonicalize 全等"锚定（与插件锚定引擎
//     findContext fuzz=0 同语义），上下文/删除行以工作区真实文件行逐字重建——
//     人类格式规范 §3"上下文行与删除行必须从工作区快照逐字复制，禁止凭记忆改写"的服务端强制；
//   - 新增行 NFC 归一 + 智能引号→ASCII + 不间断空格→空格（规范 §3 内容质量）；
//   - 任一 hunk 失配 / 文件缺失 / 路径穿越 / 含 Move to 段 / 语法坏 → 整补丁拒绝
//     （镜像插件"任一 hunk 失配整体拒绝"语义）；调用方据此置空 diff_patch，finding 本体保留。
//
// hunk 行模型为单一有序列表（ctx/del/add 交错保留位置序）——并行数组会把上下文行错位到
// 删除块之后（真实沙箱运行抓到的结构 bug），交错模型是产出可被逐段顺序应用的前提。
//
// 依据: ADR-183（人类任务指令 apply_patch 格式规范）；Cline apply-patch-parser 锚定语义。
package service

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// hunk 行类型：' '=上下文（原文件逐字） '-'=删除（原文件逐字） '+'=新增。
const (
	lineCtx = ' '
	lineDel = '-'
	lineAdd = '+'
)

// patchLine — hunk 中的一行（保留交错位置序）。
type patchLine struct {
	kind byte
	text string
}

// patchHunk — 一个改动块。
// @@ 锚点行并入首位上下文行（Cline 同款：锚点行即首条上下文行）。
type patchHunk struct {
	lines   []patchLine
	eofMark bool // *** End of File：本 hunk 须锚定文件末尾
}

// oldLines — hunk 的原文件行（上下文+删除，按序）。
func (h *patchHunk) oldLines() []string {
	out := make([]string, 0, len(h.lines))
	for _, l := range h.lines {
		if l.kind != lineAdd {
			out = append(out, l.text)
		}
	}
	return out
}

// newLines — hunk 的新行（上下文+新增，按序）。
func (h *patchHunk) newLines() []string {
	out := make([]string, 0, len(h.lines))
	for _, l := range h.lines {
		if l.kind != lineDel {
			out = append(out, l.text)
		}
	}
	return out
}

// patchSection — 补丁中的一个文件段。
type patchSection struct {
	kind  string // update | add | delete
	path  string
	hunks []patchHunk // update 用
	addLn []string    // add 用（新文件内容行）
}

// smartPunct — 与插件 canonicalize 同表的标点折叠映射（比较侧+新增行清洗侧共用）。
var smartPunct = map[rune]rune{
	'‐': '-', '‑': '-', '‒': '-', '–': '-', '—': '-', '−': '-',
	'“': '"', '”': '"', '„': '"', '«': '"', '»': '"',
	'‘': '\'', '’': '\'', '‛': '\'',
	'\u00A0': ' ', '\u202F': ' ', // NBSP / NNBSP → 空格
}

// canonicalize — NFC + 智能标点折叠 + 反转义引号（Cline apply-patch-parser.ts canonicalize
// 全表对标：比较用不改原文；插件 canonicalize 同语义）。
func canonicalize(s string) string {
	n := norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(n))
	for _, r := range n {
		if f, ok := smartPunct[r]; ok {
			b.WriteRune(f)
		} else {
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "\\`", "`")
	out = strings.ReplaceAll(out, "\\'", "'")
	out = strings.ReplaceAll(out, "\\\"", "\"")
	return out
}

// cleanAddedLine — 新增行的内容质量归一（规范 §3）：NFC + 智能引号→ASCII + NBSP→空格。
func cleanAddedLine(s string) string { return canonicalize(s) }

// NormalizeDiffPatch — 解析、校验并重建 apply_patch 补丁。
// 返回规范化补丁文本（上下文/删除行=工作区逐字行，可直接被插件引擎 fuzz=0 应用）；
// 任何失配返回 error（整补丁拒绝，绝不部分产出）。
func NormalizeDiffPatch(raw, workspaceDir string) (string, error) {
	secs, err := parseApplyPatch(raw)
	if err != nil {
		return "", err
	}
	if len(secs) == 0 {
		return "", fmt.Errorf("empty patch: no file sections")
	}
	var out strings.Builder
	out.WriteString("*** Begin Patch\n")
	for _, sec := range secs {
		switch sec.kind {
		case "update":
			hunks, err := anchorAndUpdate(sec, workspaceDir)
			if err != nil {
				return "", fmt.Errorf("update %s: %w", sec.path, err)
			}
			fmt.Fprintf(&out, "*** Update File: %s\n", sec.path)
			for i := range hunks {
				writeHunk(&out, &hunks[i])
			}
		case "add":
			if err := checkAddTarget(sec, workspaceDir); err != nil {
				return "", fmt.Errorf("add %s: %w", sec.path, err)
			}
			fmt.Fprintf(&out, "*** Add File: %s\n", sec.path)
			for _, ln := range sec.addLn {
				out.WriteString("+" + cleanAddedLine(ln) + "\n")
			}
		case "delete":
			if err := checkDeleteTarget(sec, workspaceDir); err != nil {
				return "", fmt.Errorf("delete %s: %w", sec.path, err)
			}
			fmt.Fprintf(&out, "*** Delete File: %s\n", sec.path)
		}
	}
	out.WriteString("*** End Patch")
	return out.String(), nil
}

// writeHunk — 重建后的 hunk 回写：@@ 锚点（=首条上下文行的真实文件行）+ 交错行序。
// @@ 行承载首条上下文行，不再重复输出该行（消费端把 @@ 内容作为上下文行，重复即错切）。
func writeHunk(out *strings.Builder, h *patchHunk) {
	for i, l := range h.lines {
		switch {
		case i == 0 && l.kind == lineCtx:
			out.WriteString("@@ " + l.text + "\n")
		case l.kind == lineAdd:
			out.WriteString(string(lineAdd) + cleanAddedLine(l.text) + "\n")
		default:
			out.WriteString(string(l.kind) + l.text + "\n")
		}
	}
	if h.eofMark {
		out.WriteString("*** End of File\n")
	}
}

// parseApplyPatch — 解析补丁文本为段结构（不做工作区校验）。
// 输入归一逐条对标 Cline normalizePatchInput（apply-patch.ts L105）：
// 逐行去 \r；双 sentinel 齐→切取其间（容忍前后解释文字）；双缺→剥首尾壳行
// （%%bash/apply_patch/EOF/```）+补 sentinel；仅一侧 sentinel=硬错误。
func parseApplyPatch(raw string) ([]patchSection, error) {
	lines, err := normalizePatchInput(raw)
	if err != nil {
		return nil, err
	}
	var secs []patchSection
	var cur *patchSection
	var hunk *patchHunk
	for i, ln := range lines {
		switch {
		case i == 0 || ln == "*** End Patch":
			// 首行 Begin / 尾行 End：已由前后缀断言覆盖
		case strings.HasPrefix(ln, "*** Update File:"):
			secs = append(secs, patchSection{kind: "update", path: sectionPath(ln, len("*** Update File:"))})
			cur, hunk = &secs[len(secs)-1], nil
		case strings.HasPrefix(ln, "*** Add File:"):
			secs = append(secs, patchSection{kind: "add", path: sectionPath(ln, len("*** Add File:"))})
			cur, hunk = &secs[len(secs)-1], nil
		case strings.HasPrefix(ln, "*** Delete File:"):
			secs = append(secs, patchSection{kind: "delete", path: sectionPath(ln, len("*** Delete File:"))})
			cur, hunk = &secs[len(secs)-1], nil
		case strings.HasPrefix(ln, "*** Move to:"):
			return nil, fmt.Errorf("*** Move to: unsupported (ADR-183: 插件端无重命名语义，不产出不可应用的补丁)")
		case ln == "*** End of File":
			if hunk == nil {
				return nil, fmt.Errorf("*** End of File outside hunk (line %d)", i+1)
			}
			hunk.eofMark = true
			hunk = nil
		case ln == "@@" || strings.HasPrefix(ln, "@@ "):
			// Cline parser 同款：裸 "@@" 也是合法小节标记（无锚内容，块上下文自锚定）——
			// GLM 实测产出该形态（evidence 15_glm_schema_test_args.json）
			if cur == nil || cur.kind != "update" {
				return nil, fmt.Errorf("@@ anchor outside Update File section (line %d)", i+1)
			}
			cur.hunks = append(cur.hunks, patchHunk{})
			hunk = &cur.hunks[len(cur.hunks)-1]
			if ln == "@@" {
				break // 裸 @@：无锚内容，hunk 由后续上下文/±行自锚定
			}
			anchor := strings.TrimPrefix(ln, "@@ ")
			// 锚点行=首条上下文行（Cline 同款）；后随显式同文上下文行不重复（LLM 常见双写）
			if !(len(lines) > i+1 && strings.TrimPrefix(lines[i+1], " ") == anchor && strings.HasPrefix(lines[i+1], " ")) {
				hunk.lines = append(hunk.lines, patchLine{kind: lineCtx, text: anchor})
			}
		default:
			if cur == nil {
				return nil, fmt.Errorf("content line before any section header (line %d): %q", i+1, ln)
			}
			switch {
			case strings.HasPrefix(ln, "+"):
				if cur.kind == "add" {
					cur.addLn = append(cur.addLn, strings.TrimPrefix(ln, "+"))
				} else if hunk != nil {
					hunk.lines = append(hunk.lines, patchLine{kind: lineAdd, text: strings.TrimPrefix(ln, "+")})
				} else {
					return nil, fmt.Errorf("addition line outside hunk (line %d)", i+1)
				}
			case strings.HasPrefix(ln, "-"):
				if hunk == nil {
					return nil, fmt.Errorf("deletion line outside hunk (line %d)", i+1)
				}
				hunk.lines = append(hunk.lines, patchLine{kind: lineDel, text: strings.TrimPrefix(ln, "-")})
			default:
				// Cline peek（apply-patch-parser.ts peek）语义：认不出 +/-/空格 前缀的行
				// 自动视作上下文行（补一个前导空格），不丢弃；*** 开头的未识别指令行
				// fail fast（真畸形），不静默吞。
				content := strings.TrimPrefix(ln, " ")
				if ln != "" && !strings.HasPrefix(ln, " ") {
					if strings.HasPrefix(ln, "***") {
						return nil, fmt.Errorf("malformed patch line %d (unknown *** directive): %q", i+1, ln)
					}
					content = ln // 裸行（漏写前导空格的上下文）→ 上下文
				}
				if cur.kind == "add" {
					return nil, fmt.Errorf("context line in Add File section (line %d)", i+1)
				}
				if hunk == nil {
					// 段头后的空行=格式噪声，跳过（无 @@/±行开段视 hunk 未开始）
					if ln == "" {
						continue
					}
					// 裸行开段（漏 @@）：也视作 hunk 开始，内容锚定不依赖行号
					cur.hunks = append(cur.hunks, patchHunk{})
					hunk = &cur.hunks[len(cur.hunks)-1]
				}
				hunk.lines = append(hunk.lines, patchLine{kind: lineCtx, text: content})
			}
		}
	}
	return secs, nil
}

// normalizePatchInput — 输入归一（逐条对标 Cline apply-patch.ts normalizePatchInput）：
// ①逐行去行尾 \r（CRLF 容错）；②双 sentinel 齐→切取其间（容忍补丁前后带解释文字）；
// ③双缺→剥首尾壳行（%%bash/apply_patch/EOF/```）+空行+补齐双 sentinel；④仅一侧=硬错误。
func normalizePatchInput(raw string) ([]string, error) {
	rawLines := strings.Split(raw, "\n")
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	begin, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "*** Begin Patch") && begin < 0 {
			begin = i
		}
		if strings.HasPrefix(l, "*** End Patch") {
			end = i
		}
	}
	switch {
	case begin >= 0 && end >= 0:
		if end < begin {
			return nil, fmt.Errorf("invalid patch text - incomplete sentinels (End before Begin)")
		}
		return lines[begin : end+1], nil
	case begin >= 0 || end >= 0:
		return nil, fmt.Errorf("invalid patch text - incomplete sentinels (Begin=%d End=%d)", begin, end)
	}
	// 双缺：剥首尾壳行（Cline BASH_WRAPPERS 同表）+空行，补 sentinel
	isWrapper := func(l string) bool {
		if strings.TrimSpace(l) == "" {
			return false
		}
		for _, w := range []string{"%%bash", "apply_patch", "EOF", "```"} {
			if strings.HasPrefix(l, w) {
				return true
			}
		}
		return false
	}
	s, e := 0, len(lines)
	for s < e && (isWrapper(lines[s]) || strings.TrimSpace(lines[s]) == "") {
		s++
	}
	for e > s && (isWrapper(lines[e-1]) || strings.TrimSpace(lines[e-1]) == "") {
		e--
	}
	body := lines[s:e]
	out := make([]string, 0, len(body)+2)
	out = append(out, "*** Begin Patch")
	out = append(out, body...)
	out = append(out, "*** End Patch")
	return out, nil
}

// sectionPath — 段头路径提取与清洗（拒绝绝对路径与穿越）。
func sectionPath(ln string, pfxLen int) string {
	p := strings.TrimSpace(ln[pfxLen:])
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean("/" + p))[1:] // Clean("/x/y")→"/x/y"→去首斜杠；"../a"→"../a" 仍可穿越，由 safeWsPath 拦截
}

// safeWsPath — 工作区内相对路径校验（防穿越；captureCodeContext 同款清洗+前缀断言）。
func safeWsPath(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return rel, nil
}

// anchorAndUpdate — Update File 段校验：逐 hunk 内容锚定（fuzz=0），
// 上下文/删除行以工作区真实行逐字替换（@@ 锚点行=首条上下文行，随真实行重建）。
func anchorAndUpdate(sec patchSection, workspaceDir string) ([]patchHunk, error) {
	rel, err := safeWsPath(sec.path)
	if err != nil {
		return nil, err
	}
	fileLines, err := readWsLines(workspaceDir, rel)
	if err != nil {
		return nil, err
	}
	out := make([]patchHunk, 0, len(sec.hunks))
	cursor := 0
	for i := range sec.hunks {
		h := sec.hunks[i]
		old := h.oldLines()
		if len(old) == 0 {
			return nil, fmt.Errorf("hunk #%d has no anchorable lines (need @@ anchor, context, or deletion)", i+1)
		}
		idx, best := findExactContext(fileLines, old, cursor)
		if idx < 0 {
			// 首行（@@ 锚点行）缩进漂移容错（gw-8bcf75e1 实证）：模型转写 "@@ <行>"
			// 时前导空白不可见且易丢——锚点丢 4 空格、其余行全部逐字（similarity 0.97）
			// 仍被 fuzz=0 拒，触发一整轮再生成沙箱。容错仅限首行（其余行缩进漂移仍拒：
			// 那是真改写风险）；命中后走下方逐字重建，@@ 行以工作区逐字行回写，产出
			// 补丁对消费端仍 fuzz=0（插件侧 @@ 精确层同构）。
			if aIdx, ok := findAnchorTrimmedContext(fileLines, old, cursor); ok {
				idx = aIdx
			} else {
				// Cline formatSkippedHunkFailure 语义：失败反馈要具体到"哪个 hunk、差多远、
				// 上下文长什么样"——这是再生成回合模型自纠的输入质量（ADR-183 补遗②）。
				preview := strings.Join(old, "\n")
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				return nil, fmt.Errorf("hunk #%d context not found in %s (scanned from line %d; content anchoring, fuzz=0 only; best similarity %.2f). Context:\n%s",
					i+1, rel, cursor+1, best, preview)
			}
		}
		// 逐字重建：hunk 内第 k 条非新增行 = 真实文件第 idx+k 行
		k := 0
		for j := range h.lines {
			if h.lines[j].kind != lineAdd {
				h.lines[j].text = fileLines[idx+k]
				k++
			}
		}
		if h.eofMark {
			// 文件尾锚定：匹配须延伸至最后一个真实行（容忍行尾换行 split 产生的
			// 合成空末元素——与插件 split('\n') 同口径）
			end := idx + len(old)
			atEof := end == len(fileLines) ||
				(end == len(fileLines)-1 && fileLines[end] == "")
			if !atEof {
				return nil, fmt.Errorf("hunk #%d marked *** End of File but match ends at line %d of %d",
					i+1, end, len(fileLines))
			}
		}
		cursor = idx + len(old)
		out = append(out, h)
	}
	return out, nil
}

// findExactContext — 顺序 first-hit 精确锚定（canonicalize 全等；插件 findContext fuzz=0 同语义）。
// 未命中时返回扫描区间内的最高相似度（Cline bestSimilarity 同款，供失败反馈）。
func findExactContext(fileLines, oldLines []string, start int) (int, float64) {
	need := canonicalize(strings.Join(oldLines, "\n"))
	lastStart := len(fileLines) - len(oldLines)
	best := 0.0
	for i := start; i <= lastStart; i++ {
		seg := canonicalize(strings.Join(fileLines[i:i+len(oldLines)], "\n"))
		if seg == need {
			return i, 1
		}
		if s := similarity(seg, need); s > best {
			best = s
		}
	}
	return -1, best
}

// findAnchorTrimmedContext — 首行（@@ 锚点）缩进漂移容错：首行按 canonicalize+TrimSpace
// 匹配定位，其余行仍须 canonicalize 全等（canonicalize 不动前导空格，故缩进差在此层吸收）。
// 语义对齐消费端：插件 applyPatch.ts @@ 精确层（未 trim 全等不计 fuzz）——锚定行随后被
// 逐字重建覆盖，产出不变量仍是"全部非新增行=工作区逐字行"。
func findAnchorTrimmedContext(fileLines, oldLines []string, start int) (int, bool) {
	if len(oldLines) < 1 || len(fileLines) < len(oldLines) {
		return -1, false
	}
	needHead := canonicalize(strings.TrimSpace(oldLines[0]))
	rest := canonicalize(strings.Join(oldLines[1:], "\n"))
	if rest == "" && len(oldLines) > 1 {
		return -1, false // 其余行空串不做容错锚定（空块语义留给精确层判定）
	}
	lastStart := len(fileLines) - len(oldLines)
	for i := start; i <= lastStart; i++ {
		if canonicalize(strings.TrimSpace(fileLines[i])) != needHead {
			continue
		}
		if canonicalize(strings.Join(fileLines[i+1:i+len(oldLines)], "\n")) == rest {
			return i, true
		}
	}
	return -1, false
}

// similarity — Cline calculateSimilarity 同款：(长串长度-Levenshtein)/长串长度。
func similarity(a, b string) float64 {
	longer, shorter := a, b
	if len(shorter) > len(longer) {
		longer, shorter = shorter, longer
	}
	if len(longer) == 0 {
		return 1
	}
	return (float64(len(longer)) - float64(levenshtein(shorter, longer))) / float64(len(longer))
}

// levenshtein — 经典编辑距离（Cline levenshteinDistance 同款矩阵实现）。
func levenshtein(a, b string) int {
	rows, cols := len(b)+1, len(a)+1
	m := make([]int, rows*cols)
	at := func(r, c int) int { return m[r*cols+c] }
	for i := 0; i <= len(b); i++ {
		m[i*cols] = i
	}
	for j := 0; j <= len(a); j++ {
		m[j] = j
	}
	for i := 1; i <= len(b); i++ {
		for j := 1; j <= len(a); j++ {
			if b[i-1] == a[j-1] {
				m[i*cols+j] = at(i-1, j-1)
			} else {
				m[i*cols+j] = 1 + min(at(i-1, j-1), at(i, j-1), at(i-1, j))
			}
		}
	}
	return at(len(b), len(a))
}

// readWsLines — 读工作区文件并按行切分（保留行内容，丢弃行尾符；与插件 split('\n') 同口径）。
func readWsLines(workspaceDir, rel string) ([]string, error) {
	if workspaceDir == "" {
		return nil, fmt.Errorf("workspace dir is empty")
	}
	data, err := os.ReadFile(filepath.Join(workspaceDir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("read workspace file: %w", err)
	}
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), nil
}

// checkAddTarget — Add File：目标必须不存在（已存在=语义冲突，拒绝）。
func checkAddTarget(sec patchSection, workspaceDir string) error {
	rel, err := safeWsPath(sec.path)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(filepath.Join(workspaceDir, filepath.FromSlash(rel))); serr == nil {
		return fmt.Errorf("target already exists")
	}
	if len(sec.addLn) == 0 {
		return fmt.Errorf("no content lines")
	}
	return nil
}

// checkDeleteTarget — Delete File：目标必须存在且段内无内容行。
func checkDeleteTarget(sec patchSection, workspaceDir string) error {
	rel, err := safeWsPath(sec.path)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(filepath.Join(workspaceDir, filepath.FromSlash(rel))); serr != nil {
		return fmt.Errorf("target not found: %w", serr)
	}
	if len(sec.hunks) > 0 {
		return fmt.Errorf("unexpected content lines in Delete File section")
	}
	return nil
}

// validatedDiffPatch — mapSandboxFindings 用：校验通过返回规范化补丁，失败置空+WARN（finding 保留）。
func validatedDiffPatch(taskID, raw, projectPath string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	out, err := NormalizeDiffPatch(t, projectPath)
	if err != nil {
		log.Printf("[fixpatch][%s] diff_patch rejected (dropped, finding kept): %v", taskID, err)
		emitTaskLog(taskID, "warn", "fixpatch",
			"diff_patch 校验失败已丢弃（finding 保留，不编造补丁）: "+err.Error())
		return ""
	}
	return out
}
