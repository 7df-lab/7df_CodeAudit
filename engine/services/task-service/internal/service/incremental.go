// incremental.go — 增量扫描核心（ADR-225）。
//
// 设计依据: 伞仓 docs/designs/incremental-scan.md（v5）：
//   §4.2 基线选定（同项目最近 COMPLETED 且源码可达；显式指定创建时已校验）
//   §4.3 内容 diff（两侧剥壳对齐 → 文件级 sha256 → changed/deleted）
//   §4.4 快照回写任务自身（重试不重算，ADR-203 哲学延续）
//   §4.7 增量 unified diff 生成（AI 聚焦提示词素材，D3）
//   §4.8 降级矩阵（无基线/基线不可达/diff 失败 → 记 incremental_degraded_reason 后全量继续）
//   D5   diff_hint 交叉核对——客户端 git 提示与服务端结果不一致仅记任务日志，不作依据。
//
// 依赖现状: 源码树按任务隔离且终态保留（archive.go 解包于 uploads-<task_id>/unpacked，
// repo_fetch clone 于 <task_id>）；本模块只在 Prepare 瞬间读一次基线树，产物全部
// 落任务记录（PG payload），此后不依赖目录存活（卷 GC 因此不破坏增量任务）。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// incrementalDiffFileName — 大 patch 落项目树的文件名（ADR-225 §4.7）：
	// dsh-runtime 写入、随 tar 进沙箱、prompt 指路；diff 遍历排除同名文件，
	// 防其混入下一轮基线对比。
	incrementalDiffFileName = ".codeaudit-incremental.diff"

	// maxIncrementalDiffBytes — incremental_diff 生成总量上限（proto 消息体保护）。
	maxIncrementalDiffBytes = 256 << 10

	// maxDiffFileBytes / maxDiffFileLines — 单文件参与行级 diff 的上限
	//（超限文件在 diff 中只留头部说明，不参与 LCS——O(N·M) 防护）。
	maxDiffFileBytes = 512 << 10
	maxDiffFileLines = 3000

	// maxDiffHintBytes — diff_hint 落任务 config 的截断上限（config 进 PG payload）。
	maxDiffHintBytes = 64 << 10

	// diffContextLines — unified diff hunk 上下文行数（git diff 惯例 3）。
	diffContextLines = 3
)

// diffDirExcludes — 内容 diff 的目录排除（与沙箱 tarProject/walkExcludes 同源的
// 最小集：仅 .git——其余目录两侧树口径一致无需排除；跨类型基线的 .git 噪声必须排除）。
var diffDirExcludes = map[string]bool{".git": true}

// normalizeRepoPath — 相对项目根路径规范化（正斜杠、去 ./、Clean）。
// 服务端 diff 产物与 result-service 继承排除清单共用此口径（ADR-225 §9-1：
// changed/deleted 与 findings.file_path 必须同口径比对）。
func normalizeRepoPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return p
	}
	return cleanSlashPath(p)
}

// cleanSlashPath — slash 路径 Clean（去空段/./、消 ../；不引入双包语义）。
func cleanSlashPath(p string) string {
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// treeDigest — 遍历 root，产出 相对路径(规范化) → 内容 sha256。
// 只收常规文件；排除 diffDirExcludes 目录与 incrementalDiffFileName；
// 文件数超 maxArchiveFiles（解包同源上限）时报错（降级路径）。
func treeDigest(root string) (map[string]string, error) {
	digest := make(map[string]string)
	n := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if diffDirExcludes[info.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Name() == incrementalDiffFileName {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		h, herr := fileSha256(p)
		if herr != nil {
			// 单文件读失败（权限/并发删除）：内容不可信 → 记特殊摘要参与对比，
			// 保证"读不出来的文件"在两侧不一致时必然进 changed/deleted。
			digest[normalizeRepoPath(rel)] = fmt.Sprintf("<unreadable:%v>", herr)
			return nil
		}
		digest[normalizeRepoPath(rel)] = h
		n++
		if n > maxArchiveFiles {
			return fmt.Errorf("文件数超过 diff 上限 %d", maxArchiveFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return digest, nil
}

func fileSha256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// contentDiff — 两棵树的文件级差异：changed=新增∪修改（新树有且内容异），
// deleted=仅基线有。结果排序保证确定性（快照/测试/审计稳定）。
func contentDiff(base, cur map[string]string) (changed, deleted []string) {
	for p, h := range cur {
		bh, ok := base[p]
		if !ok || bh != h {
			changed = append(changed, p)
		}
	}
	for p := range base {
		if _, ok := cur[p]; !ok {
			deleted = append(deleted, p)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted
}

// ---- unified diff 生成（AI 聚焦提示词素材，ADR-225 §4.7）----

// unifiedDiffTrees — 变更文件的行级 unified diff（上下文 3 行）+ 删除文件头部。
// 单文件超限（字节/行数）只留说明；总量超 maxIncrementalDiffBytes 截断并注明。
func unifiedDiffTrees(baseRoot, newRoot string, changed, deleted []string) string {
	var b strings.Builder
	truncated := false
	for _, p := range changed {
		if b.Len() >= maxIncrementalDiffBytes {
			truncated = true
			break
		}
		b.WriteString(unifiedDiffFile(filepath.Join(baseRoot, filepath.FromSlash(p)),
			filepath.Join(newRoot, filepath.FromSlash(p)), p))
	}
	for _, p := range deleted {
		if b.Len() >= maxIncrementalDiffBytes {
			truncated = true
			break
		}
		fmt.Fprintf(&b, "--- %s\n+++ /dev/null（本次扫描已删除）\n", p)
	}
	if truncated {
		fmt.Fprintf(&b, "\n（diff 超过 %dKB 上限已截断；完整变更以任务 changed_files/deleted_files 清单为准）\n",
			maxIncrementalDiffBytes>>10)
	}
	return b.String()
}

// unifiedDiffFile — 单文件 unified diff。超限/不可读/二进制（含 NUL）时返回说明头。
func unifiedDiffFile(basePath, newPath, rel string) string {
	baseLines, baseSkip := readDiffLines(basePath)
	newLines, newSkip := readDiffLines(newPath)
	if baseSkip != "" || newSkip != "" {
		note := baseSkip
		if note == "" {
			note = newSkip
		}
		return fmt.Sprintf("--- %s\n+++ %s\n@@ 文件 diff 略过：%s\n", rel, rel, note)
	}
	hunks := diffHunks(baseLines, newLines)
	if hunks == nil { // 完全一致（hash 冲突理论场景）：说明头兜底
		return fmt.Sprintf("--- %s\n+++ %s\n@@ 内容一致\n", rel, rel)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", rel, rel)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldLen, h.newStart, h.newLen)
		for _, l := range h.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// readDiffLines — 读文件按行切分；返回 (lines, skipReason)。skipReason 非空=不参与 LCS。
func readDiffLines(p string) ([]string, string) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, "文件不可读（" + relErrNote(err) + "）"
	}
	if fi.Size() > maxDiffFileBytes {
		return nil, fmt.Sprintf("文件超过 %dKB 上限", maxDiffFileBytes>>10)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "文件不可读"
	}
	if idx := indexByte(data, 0); idx >= 0 {
		return nil, "二进制文件"
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > maxDiffFileLines {
		return nil, fmt.Sprintf("行数超过 %d 上限", maxDiffFileLines)
	}
	return lines, ""
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func relErrNote(err error) string {
	if os.IsNotExist(err) {
		return "新增文件"
	}
	return "读取失败"
}

type diffHunk struct {
	oldStart, oldLen, newStart, newLen int
	lines                              []string
}

// diffHunks — LCS 行级差异 → 带 3 行上下文的 unified hunks。
// O(N·M) 动态规划（maxDiffFileLines=3000 上界内可控）；nil=内容完全一致。
func diffHunks(a, b []string) []diffHunk {
	n, m := len(a), len(b)
	// LCS 表（uint32 足够：3000×3000=9e6 < 2^32）
	lcs := make([][]uint32, n+1)
	for i := range lcs {
		lcs[i] = make([]uint32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	// 回溯产出编辑脚本（-old / +new / ' ' 上下文）
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	ops := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	if len(ops) == n+m && n == m { // 全上下文（无任何增删）→ 一致
		onlyCtx := true
		for _, o := range ops {
			if o.kind != ' ' {
				onlyCtx = false
				break
			}
		}
		if onlyCtx {
			return nil
		}
	}
	// 变更点（非上下文 op）扩展上下文窗口 → hunks
	changedIdx := map[int]bool{}
	for k, o := range ops {
		if o.kind != ' ' {
			for c := k - diffContextLines; c <= k+diffContextLines; c++ {
				if c >= 0 && c < len(ops) {
					changedIdx[c] = true
				}
			}
		}
	}
	var hunks []diffHunk
	k := 0
	oldNo, newNo := 1, 1 // 行号从 1 计
	for k < len(ops) {
		if !changedIdx[k] {
			if ops[k].kind != '+' {
				oldNo++
			}
			if ops[k].kind != '-' {
				newNo++
			}
			k++
			continue
		}
		// hunk 起点
		h := diffHunk{oldStart: oldNo, newStart: newNo}
		for k < len(ops) && changedIdx[k] {
			switch ops[k].kind {
			case ' ':
				h.lines = append(h.lines, " "+ops[k].text)
				h.oldLen++
				h.newLen++
				oldNo++
				newNo++
			case '-':
				h.lines = append(h.lines, "-"+ops[k].text)
				h.oldLen++
				oldNo++
			case '+':
				h.lines = append(h.lines, "+"+ops[k].text)
				h.newLen++
				newNo++
			}
			k++
		}
		hunks = append(hunks, h)
	}
	return hunks
}

// ---- 基线选定与 Prepare 包装（§4.2/§4.3/§4.8）----

// baselineTreePath — 基线任务源码树在卷上的位置（上传型 uploads-<id>/unpacked、
// repo 型 <id>），存在则返回剥壳后的根；不存在返回 ""。
func (s *TaskServiceImpl) baselineTreePath(b *pb.ScanTask) string {
	for _, cand := range []string{
		filepath.Join(s.reposDir, "uploads-"+b.GetTaskId(), "unpacked"),
		filepath.Join(s.reposDir, b.GetTaskId()),
	} {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return ResolveProjectRoot(cand)
		}
	}
	return ""
}

// selectBaseline — 显式（创建时已校验存在/同项目/COMPLETED）或自动选定：
// 同项目 COMPLETED 任务中 created_at 最新且源码可达者（卷树在位，或持有
// upload_file_id 可三级重物化）。回退更早基线语义安全（差异面只大不小）。
func (s *TaskServiceImpl) selectBaseline(task *pb.ScanTask) *pb.ScanTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id := task.GetBaselineTaskId(); id != "" {
		if b, ok := s.tasks[id]; ok && b.GetProjectId() == task.GetProjectId() {
			return b
		}
		return nil // 显式基线在启动时消失（如重启丢内存态）——不可静默换基线
	}
	var best *pb.ScanTask
	for _, b := range s.tasks {
		if b.GetTaskId() == task.GetTaskId() ||
			b.GetProjectId() != task.GetProjectId() ||
			b.GetStatus() != pb.TaskStatus_TASK_STATUS_COMPLETED {
			continue
		}
		if s.baselineTreePath(b) == "" && b.GetConfig()["upload_file_id"] == "" && b.GetConfig()["tree_tar_file_id"] == "" {
			continue // 源码不可达（无卷树、无树 tar 锚点、亦无上传原件可重物化）
		}
		if best == nil ||
			b.GetCreatedAt().AsTime().After(best.GetCreatedAt().AsTime()) ||
			(b.GetCreatedAt().AsTime().Equal(best.GetCreatedAt().AsTime()) && b.GetTaskId() > best.GetTaskId()) {
			best = b
		}
	}
	return best
}

// rehydrateBaselineTar — 重物化层②（ADR-225 §4.2/S5）：基线任务的树 tar 锚点
//（config.tree_tar_file_id，trees/<id>.tar.gz 剥壳根快照）→ 下载解包 scratch。
// tar 即剥壳根，无需再剥；失败返回 err 由调用者降级到层③。
func (s *TaskServiceImpl) rehydrateBaselineTar(ctx context.Context, b *pb.ScanTask, tarFileID, taskID string) (string, func(), error) {
	storageAddr := envOr("CODEAUDIT_STORAGE_ADDR", "")
	if storageAddr == "" {
		return "", nil, fmt.Errorf("CODEAUDIT_STORAGE_ADDR 未配置")
	}
	conn, err := grpc.NewClient(storageAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", nil, err
	}
	defer conn.Close()
	dlCtx, dlCancel := context.WithTimeout(ctx, downloadTimeout) // R63: 对齐 archive.go 120s 口径（原裸 ctx 可无限阻塞）
	defer dlCancel()
	stream, err := pb.NewStorageServiceClient(conn).DownloadFile(dlCtx, &pb.DownloadFileRequest{FileId: tarFileID})
	if err != nil {
		return "", nil, err
	}
	scratchRoot := filepath.Join(s.reposDir, fmt.Sprintf("rehy-%s-%s", taskID, b.GetTaskId()))
	unpacked := filepath.Join(scratchRoot, "unpacked")
	_ = os.RemoveAll(scratchRoot)
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		return "", nil, err
	}
	archivePath := filepath.Join(scratchRoot, "tree.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		_ = os.RemoveAll(scratchRoot)
		return "", nil, err
	}
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			_ = os.RemoveAll(scratchRoot)
			return "", nil, rerr
		}
		if _, werr := f.Write(chunk.GetData()); werr != nil {
			f.Close()
			_ = os.RemoveAll(scratchRoot)
			return "", nil, werr
		}
	}
	if err := f.Close(); err != nil {
		_ = os.RemoveAll(scratchRoot)
		return "", nil, err
	}
	if err := unpackTreeTarGz(archivePath, unpacked); err != nil {
		_ = os.RemoveAll(scratchRoot)
		return "", nil, err
	}
	_ = os.Remove(archivePath)
	return unpacked, func() { _ = os.RemoveAll(scratchRoot) }, nil
}

// rehydrateBaseline — 重物化层③（ADR-225 §4.2）：卷树不在位时从基线任务的
// 上传原件（uploads 桶）重解包+剥壳到 scratch；返回 (根, cleanup, err)。
func (s *TaskServiceImpl) rehydrateBaseline(ctx context.Context, b *pb.ScanTask, taskID string) (string, func(), error) {
	uploadID := b.GetConfig()["upload_file_id"]
	if uploadID == "" {
		return "", nil, fmt.Errorf("基线 %s 无卷树且无上传原件可重物化", b.GetTaskId())
	}
	storageAddr := envOr("CODEAUDIT_STORAGE_ADDR", "")
	if storageAddr == "" {
		return "", nil, fmt.Errorf("CODEAUDIT_STORAGE_ADDR 未配置，无法重物化基线 %s", b.GetTaskId())
	}
	scratch := filepath.Join(s.reposDir, fmt.Sprintf("rehy-%s-%s", taskID, b.GetTaskId()))
	root, err := FetchUploadArchive(ctx, storageAddr, uploadID, scratch)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return "", nil, err
	}
	return root, func() { _ = os.RemoveAll(scratch) }, nil
}

// wrapIncrementalPrepare — 增量 Prepare 包装（StartTask 装配，编排协程内执行）。
// 原 Prepare/静态路径产出新树根后跑增量解析；结果快照回写任务自身。
func (s *TaskServiceImpl) wrapIncrementalPrepare(taskID string, basePrepare func(context.Context) (string, error), staticPath string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var p string
		var err error
		if basePrepare != nil {
			p, err = basePrepare(ctx)
		} else {
			p = staticPath
		}
		if err != nil {
			return "", err
		}
		s.runIncrementalDiff(ctx, taskID, p)
		return p, nil
	}
}

// runIncrementalDiff — 增量解析主体（幂等：快照已在则直接复用，重试不重算）。
func (s *TaskServiceImpl) runIncrementalDiff(ctx context.Context, taskID, newPath string) {
	inc := s.currentIncremental(taskID)
	if inc == nil {
		return
	}
	// 快照复用（ADR-225 A5.2）：首启已算过 → 直接填充编排上下文。
	// 同进程重试（自动重试二次进 Prepare）时 inc 已激活——保留首轮 diffText
	//（AI 提示词素材不丢）；进程重启后的重试只有快照清单，diffText 空（提示词降级
	// 为仅清单，语义仍正确）。
	if a, _, _, _, _ := inc.Snapshot(); a {
		return
	}
	s.mu.RLock()
	task := s.tasks[taskID]
	if task != nil && task.GetDiffSource() != "" {
		snap := cloneLocked(task)
		s.mu.RUnlock()
		inc.Set(true, snap.GetBaselineTaskId(), snap.GetChangedFiles(), snap.GetDeletedFiles(), "")
		return
	}
	intent := ""
	if task != nil {
		intent = task.GetConfig()["incremental"]
	}
	s.mu.RUnlock()
	if intent != "true" {
		return
	}

	baseline := s.selectBaseline(s.snapshotTask(taskID))
	if baseline == nil {
		s.degradeIncremental(taskID, "no_baseline", inc)
		return
	}
	baseRoot := s.baselineTreePath(baseline)
	cleanup := func() {}
	if baseRoot == "" {
		// 层②（树 tar，S5）→ 层③（上传原件）逐级尝试（ADR-225 §4.2 重物化序列）
		root, clean, err := (func() (string, func(), error) {
			if tarID := baseline.GetConfig()["tree_tar_file_id"]; tarID != "" {
				if r, c, e := s.rehydrateBaselineTar(ctx, baseline, tarID, taskID); e == nil {
					return r, c, nil
				} else {
					log.Printf("[task %s] 基线树 tar 重物化失败，降级上传原件层: %v", taskID, e)
				}
			}
			return s.rehydrateBaseline(ctx, baseline, taskID)
		})()
		if err != nil {
			s.degradeIncremental(taskID, "baseline_unavailable: "+err.Error(), inc)
			return
		}
		baseRoot, cleanup = root, clean
	}
	defer cleanup()

	baseDigest, err1 := treeDigest(baseRoot)
	newDigest, err2 := treeDigest(newPath)
	if err1 != nil || err2 != nil {
		reason := "diff_failed:"
		if err1 != nil {
			reason += " baseline: " + err1.Error()
		}
		if err2 != nil {
			reason += " current: " + err2.Error()
		}
		s.degradeIncremental(taskID, reason, inc)
		return
	}
	changed, deleted := contentDiff(baseDigest, newDigest)
	diffText := unifiedDiffTrees(baseRoot, newPath, changed, deleted)

	s.mu.Lock()
	cur, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	cur.BaselineTaskId = baseline.GetTaskId()
	cur.ChangedFiles = changed
	cur.DeletedFiles = deleted
	cur.DiffSource = "content"
	s.appendLogLocked(taskID, pb.TaskLogLevel_TASK_LOG_LEVEL_INFO, "task",
		fmt.Sprintf("增量扫描：基线 %s（changed=%d deleted=%d）", baseline.GetTaskId(), len(changed), len(deleted)))
	s.persistTaskLocked(cur)
	notify := cur.GetTaskId()
	s.mu.Unlock()
	s.hub.notify(notify) // ADR-189

	s.crossCheckHint(taskID, changed, deleted)
	inc.Set(true, baseline.GetTaskId(), changed, deleted, diffText)
	s.emitIncrementalScopeNotice(taskID, inc) // A6.3/R59: 激活点发视野声明（Execute 前调用时 inc 恒空壳→永不可达）
}

// currentIncremental — 取任务挂载的增量上下文（StartTask 装配时登记）。
func (s *TaskServiceImpl) currentIncremental(taskID string) *orchestrator.IncrementalContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.incrementalCtx[taskID]
}

// snapshotTask — 任务深拷贝（锁内克隆语义与 GetScanTask 一致，ADR-131）。
func (s *TaskServiceImpl) snapshotTask(taskID string) *pb.ScanTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.tasks[taskID]; ok {
		return cloneLocked(t)
	}
	return &pb.ScanTask{}
}

// degradeIncremental — 降级（§4.8）：记诊断键+日志后按全量继续（inc 不激活）。
func (s *TaskServiceImpl) degradeIncremental(taskID, reason string, inc *orchestrator.IncrementalContext) {
	inc.Set(false, "", nil, nil, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tasks[taskID]
	if !ok {
		return
	}
	if cur.Config == nil {
		cur.Config = map[string]string{}
	}
	cur.Config["incremental_degraded_reason"] = reason
	s.appendLogLocked(taskID, pb.TaskLogLevel_TASK_LOG_LEVEL_WARN, "task",
		fmt.Sprintf("增量不可用，已降级全量（%s）", reason))
	s.persistTaskLocked(cur)
	s.hub.notify(taskID)
	log.Printf("[task %s] incremental degraded: %s", taskID, reason)
}

// crossCheckHint — D5: 客户端 git diff --name-status 提示与服务端结果集合比对；
// 不一致仅记任务日志（审计客户端健康），changed/deleted 恒以服务端为准。
func (s *TaskServiceImpl) crossCheckHint(taskID string, changed, deleted []string) {
	s.mu.RLock()
	hint := ""
	if t, ok := s.tasks[taskID]; ok {
		hint = t.GetConfig()["diff_hint"]
	}
	s.mu.RUnlock()
	if strings.TrimSpace(hint) == "" {
		return
	}
	server := map[string]bool{}
	for _, p := range changed {
		server[p] = true
	}
	for _, p := range deleted {
		server[p] = true
	}
	var mismatches []string
	for _, line := range strings.Split(strings.TrimSpace(hint), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || len(fields[0]) < 1 {
			continue
		}
		// git name-status: A/M/D/Rxx <path> [newpath]；R 视为删旧+增新
		paths := fields[1:]
		switch fields[0][0] {
		case 'R':
			paths = fields[1:] // old + new 都在
		}
		for _, p := range paths {
			np := normalizeRepoPath(p)
			if np != "" && !server[np] {
				mismatches = append(mismatches, np)
			}
		}
	}
	if len(mismatches) == 0 {
		return
	}
	sort.Strings(mismatches)
	if len(mismatches) > 8 {
		mismatches = append(mismatches[:8], "…")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLogLocked(taskID, pb.TaskLogLevel_TASK_LOG_LEVEL_WARN, "task",
		fmt.Sprintf("客户端 git 提示与服务端 diff 不一致（以服务端为准）：%s", strings.Join(mismatches, ", ")))
}
