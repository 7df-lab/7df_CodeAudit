// fixpatch — ADR-183: diff_patch 服务端校验与规范化。
//
// 沙箱 DSH 产出的 apply_patch 补丁文本经本模块校验重建后才允许进入 UnifiedFinding.diff_patch：
//   - Update 段 "@@ 定义行"按 Cline apply-patch-parser 语义作寻位指令（canonTrim/trim
//     容错匹配，不物化为 hunk 内容——R37 对齐插件 applyPatch.ts 逐字移植的上游行为），
//     hunk 锚定依赖显式上下文/删除行；
//   - Update 段按"顺序游标 + first-hit + NFC canonicalize 全等"锚定（与插件锚定引擎
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
// defStr — "@@ <行>" 的定义行内容（Cline apply-patch-parser 语义：寻位指令，不物化为
// hunk 内容；锚定只依赖显式上下文/删除行——R37 对齐插件 applyPatch.ts 逐字移植的上游行为）。
type patchHunk struct {
	defStr string         // "@@ " 后的原文；bare "@@" 为空串
	lines  []patchLine    // 交错保留位置序
	eofMark bool          // *** End of File：本 hunk 须锚定文件末尾
	// anchorLine — 重建时显式写出的 @@ 行（规范化路径设置：带删除行 hunk 取变更块
	// 上一行；文件顶改动取首条变更行本身）。空=沿用 lines[0] 首条上下文行惯例。
	anchorLine string
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
	for i := range secs {
		secs[i].path = resolveSectionPath(workspaceDir, secs[i].path)
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
	if h.anchorLine != "" {
		// anchorLine 路径（idx==0 形态，lines[0] 是删除行）——@@ 显式写出，
		// 首条 ctx 行不再并入 @@（下方条件以 anchorLine == "" 为前提，防双写）
		out.WriteString("@@ " + h.anchorLine + "\n")
	}
	for i, l := range h.lines {
		switch {
		case i == 0 && l.kind == lineCtx && h.anchorLine == "":
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
			// Cline apply-patch-parser 语义（R37 对齐插件 applyPatch.ts 逐字移植）：
			// @@ 定义行只作寻位（defStr），不物化为 hunk 内容——锚定只依赖显式
			// 上下文/删除行；锚点丢缩进/双写/夹新增等模型自然书写形态不再整补丁被拒。
			if cur == nil || cur.kind != "update" {
				return nil, fmt.Errorf("@@ anchor outside Update File section (line %d)", i+1)
			}
			cur.hunks = append(cur.hunks, patchHunk{})
			hunk = &cur.hunks[len(cur.hunks)-1]
			if ln != "@@" {
				hunk.defStr = strings.TrimPrefix(ln, "@@ ")
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

// sandboxPathPrefixes — 沙箱挂载视角的路径前缀（按前缀长度降序：先剥长前缀）。
// 模型在沙箱内看到的项目根是 /sandbox/project，产出补丁时常把段路径写成该挂载视角
// （"/sandbox/project/x" 经 sectionPath 清洗后形如 "sandbox/project/x"，或直接 "project/x"），
// 而校验与消费两侧都按工作区根（=项目根）解析（实证 13/13 补丁因此被拒）。
var sandboxPathPrefixes = []string{"sandbox/project/", "project/"}

// resolveSectionPath — 段路径的沙箱视角容错：原路径在工作区不存在且带挂载前缀时，
// 改写为剥前缀形态（产出补丁同步改写，消费端按工作区根应用）。
//   - update/delete：以"目标文件存在"为准（原路径存在=真有该目录，不动——防误剥合法
//     顶层 project/ 目录的仓）；
//   - add：目标必须不存在，改以"父目录存在"为准；
//   - 全部候选都不存在时原样返回，让后续锚定错误携带原路径（失败反馈保真）。
func resolveSectionPath(workspaceDir, path string) string {
	if workspaceDir == "" || path == "" {
		return path
	}
	trimmed := path
	for _, pfx := range sandboxPathPrefixes {
		if strings.HasPrefix(trimmed, pfx) {
			trimmed = strings.TrimPrefix(trimmed, pfx)
			break
		}
	}
	if trimmed == path {
		return path // 无挂载前缀（或恰为剥净后的空串），无容错余地
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(workspaceDir, filepath.FromSlash(rel)))
		return err == nil
	}
	parentExists := func(rel string) bool {
		dir := filepath.Dir(filepath.Join(workspaceDir, filepath.FromSlash(rel)))
		fi, err := os.Stat(dir)
		return err == nil && fi.IsDir()
	}
	if !exists(path) && exists(trimmed) {
		return trimmed
	}
	// add 语义：两个目标都不存在时，父目录在者胜（原路径父目录在=模型本意即原路径）
	if !exists(path) && !exists(trimmed) && parentExists(trimmed) && !parentExists(path) {
		return trimmed
	}
	return path
}

// anchorAndUpdate — Update File 段校验：逐 hunk 内容锚定（fuzz=0），
// 上下文/删除行以工作区真实行逐字替换。
// defStr 语义对齐 Cline（插件 applyPatch.ts 逐字移植）：先三级容错寻位（canonTrim/trim，
// 兼容锚点丢缩进），游标推进到命中行（INCLUSIVE——双写形态的显式上下文行就是 defStr
// 行本身）；defStr 未命中不判死，降级为纯内容锚定（显式行自足时照常通过）。
// 纯新增 hunk（无显式上下文/删除行）：defStr 命中行后插入（重建为 canonical
// "@@ 真实行 + 新增"形态）；无 defStr 则位置歧义，如实拒绝。
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
			// 纯新增 hunk：defStr 提供插入位置
			if strings.TrimSpace(h.defStr) == "" {
				return nil, fmt.Errorf("hunk #%d has no anchorable lines (need @@ anchor with content, context, or deletion)", i+1)
			}
			idx, ok := seekDefStr(fileLines, h.defStr, cursor)
			if !ok {
				return nil, fmt.Errorf("hunk #%d pure addition: @@ anchor %q not found in %s", i+1, h.defStr, rel)
			}
			if h.eofMark {
				// 插入点须在文件尾（同 del/ctx hunk 的 EOF 口径，容忍合成空末元素）
				at := idx + 1
				atEof := at == len(fileLines) ||
					(at == len(fileLines)-1 && fileLines[at] == "")
				if !atEof {
					return nil, fmt.Errorf("hunk #%d marked *** End of File but insertion lands at line %d of %d",
						i+1, at, len(fileLines))
				}
			}
			ins := make([]patchLine, 0, len(h.lines)+1)
			ins = append(ins, patchLine{kind: lineCtx, text: fileLines[idx]}) // 重建 canonical "@@ 真实行 + 新增"
			ins = append(ins, h.lines...)
			h.lines = ins
			cursor = idx + 1
			out = append(out, h)
			continue
		}
		// defStr 寻位（hint，尽力而为）：双写/丢缩进场景显式行起始于 defStr 行本身
		if strings.TrimSpace(h.defStr) != "" {
			if j, ok := seekDefStr(fileLines, h.defStr, cursor); ok {
				cursor = j
			}
			// 未命中：defStr 幻觉不判死——显式行内容锚定自足（R37 形态三）
		}
		idx, best := findExactContext(fileLines, old, cursor)
		if idx < 0 {
			// 首行（@@ 锚点行）缩进漂移容错（实证）：模型转写 "@@ <行>"
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
				hintNote := ""
				if strings.TrimSpace(h.defStr) != "" {
					hintNote = fmt.Sprintf("; @@ anchor %q not matched either", h.defStr)
				}
				return nil, fmt.Errorf("hunk #%d context not found in %s (scanned from line %d; content anchoring, fuzz=0 only; best similarity %.2f%s). Context:\n%s",
					i+1, rel, cursor+1, best, hintNote, preview)
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
		// 规范化重建（R37）：defStr 不再物化为上下文行后，纯 del/add hunk 无 ctx 首行，
		// writeHunk 会丢 @@——分两形态：
		//   a) 纯插入（无删除行）且新增行在显式上下文之前（实证形态）：语义=
		//      紧邻锚定行插入，重建为 canonical "@@ 锚定真实行 + 新增"（消费端 seek 后
		//      纯插入，落点一致；ctx 回声行随真实行重建不再重复）；
		//   b) 其余（含删除行）：从真实文件取变更块上一行前置（消费端 seek 过该行恰落 idx）。
		// idx==0 且带删除行（文件顶改动且无上下文）在该补丁语法下不可表达，如实拒绝。
		if h.lines[0].kind != lineCtx {
			hasDel := false
			for _, l := range h.lines {
				if l.kind == lineDel {
					hasDel = true
					break
				}
			}
			switch {
			case !hasDel:
				ins := []patchLine{{kind: lineCtx, text: fileLines[idx]}}
				for _, l := range h.lines {
					if l.kind == lineAdd {
						ins = append(ins, l)
					}
				}
				h.lines = ins
			case idx == 0:
				// 文件顶改动无"上一行"可前置：@@ 直接承载首条变更行本身
				// （实证形态；消费端插件 findContext 全文回扫兜底层覆盖）
				h.anchorLine = fileLines[idx]
			default:
				h.lines = append([]patchLine{{kind: lineCtx, text: fileLines[idx-1]}}, h.lines...)
			}
		}
		cursor = idx + len(old)
		out = append(out, h)
	}
	return out, nil
}

// seekDefStr — @@ 定义行寻位（Cline defStr 三级匹配的平台侧两级收敛）：
// canonTrim 全等（锚点裸写丢缩进即精确命中）→ 文件行 trim 后全等（缩进漂移容错）。
// 自 cursor 起扫，未命中回退全文（锚点幻觉/乱序段不判死，调用方降级内容锚定）。
// 返回命中行下标——INCLUSIVE：双写形态的显式上下文行就是 defStr 行本身。
func seekDefStr(fileLines []string, defStr string, cursor int) (int, bool) {
	want := canonicalize(strings.TrimSpace(defStr))
	if want == "" {
		return 0, false
	}
	for _, from := range []int{cursor, 0} {
		for i := from; i < len(fileLines); i++ {
			if canonicalize(fileLines[i]) == want || canonicalize(strings.TrimSpace(fileLines[i])) == want {
				return i, true
			}
		}
	}
	return 0, false
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
