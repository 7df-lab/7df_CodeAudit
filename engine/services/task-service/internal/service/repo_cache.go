// repo_cache.go — 数据面：桶为持久 SSOT，共享卷为可丢弃缓存（ADR-225 D6）。
//
// 设计依据: 伞仓 docs/designs/incremental-scan.md §4.10（丢弃时机）：
//   F12 树 tar 入桶：Prepare 产出的剥壳根打成 trees/<task_id>.tar.gz 流式上传
//       storage（trees 域桶），成功后 file_id 回写任务 config.tree_tar_file_id——
//       持久锚点兼 GC 删除前提与 gateway 重物化依据；失败不阻塞主流程（诚实记账）。
//   F13 卷缓存丢弃（三条触发线 + 硬保护，执行者=周期对账回收器，ADR-210 同模式）：
//       ①TTL：终态 && tar 在桶（HEAD 校验）&& 距终态超过 repo_cache_ttl_s → 丢弃
//       ②容量：卷用量超 repo_cache_max_bytes → 终态任务旧→新强制驱逐（tar 前提不变）
//       ③孤儿：目录在但任务记录不在 → repo_cache_orphan_ttl_s 后清理+记账
//       硬保护：运行中/PAUSED 任务的树绝不删；storage=memory 档全线停用
//       （GetStorageMode 探测——memory 档对象在内存重启即失，删卷树=数据不可恢复）；
//       ai-interaction（R36 共享卷邻居）与 .gateway-cache（gateway 重物化缓存）绝不触碰；
//       重试不依赖旧树（cloneRepo/archive 先清残留重建），驱逐不影响重试语义；
//       删除动作=先原子改名隔离（.gc-<ts>）再 rm（并发读者句柄不断流）。
package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/codeaudit/proto-gen"
)

// treeTarUploadTimeout — 树 tar 入桶全链上限（打包+流式上传）。
const treeTarUploadTimeout = 10 * time.Minute

// treeTarChunkBytes — UploadFile 客户端流分块（与 gateway 上传同款 64KiB）。
const treeTarChunkBytes = 64 << 10

// unpackTreeTarGz — tar.gz 解包（ADR-225 基线重物化层② 消费面）。与 gateway
// sourcefile_rehydrate.go 同语义跨模块复制件（穿越吸收/软链跳过/总量与文件数上限
// 同 task archive.go 口径）——修改时两处同步。
func unpackTreeTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	count := 0
	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			return fmt.Errorf("read tar: %w", herr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean := strings.ReplaceAll(filepath.Clean("/"+hdr.Name), "\\", "/")
		clean = strings.TrimPrefix(clean, "/")
		target := filepath.Join(dest, filepath.FromSlash(clean))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("path traversal entry rejected: %s", hdr.Name)
		}
		if total+hdr.Size > maxUnpackedBytes {
			return fmt.Errorf("解包总量超过 %dMB", maxUnpackedBytes>>20)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, oerr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if oerr != nil {
			return oerr
		}
		n, cerr := io.Copy(out, tr)
		out.Close()
		if cerr != nil {
			return cerr
		}
		total += n
		count++
		if count > maxArchiveFiles {
			return fmt.Errorf("解包文件数超过 %d", maxArchiveFiles)
		}
	}
	if count == 0 {
		return fmt.Errorf("树 tar 内没有文件")
	}
	return nil
}

// gcProtectedNames — 回收器绝不触碰的卷内邻居目录（前缀/名字双口径）：
// ai-interaction=R36 AI 交互日志共享卷；.gateway-cache=gateway 重物化缓存（gateway 自管 LRU）。
var gcProtectedNames = []string{"ai-interaction", ".gateway-cache"}

// uploadTreeTar — 剥壳根打 tar.gz（排除 .git 与 .codeaudit-incremental.diff，与
// 内容 diff 同口径）流式上传 storage；成功后回写 task.Config["tree_tar_file_id"]。
// 异步调用（编排协程外）；一次任务只入桶一次（重试复用既有锚点，防重复孤儿对象）。
func (s *TaskServiceImpl) uploadTreeTar(taskID, root string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[task %s] tree tar panic（不影响任务主流程）: %v", taskID, r)
		}
	}()
	s.mu.RLock()
	task, ok := s.tasks[taskID]
	existing := ""
	if ok {
		existing = task.GetConfig()["tree_tar_file_id"]
	}
	s.mu.RUnlock()
	if !ok || existing != "" {
		return // 任务已不在/已入桶过（幂等）
	}
	storageAddr := envOr("CODEAUDIT_STORAGE_ADDR", "")
	if storageAddr == "" {
		log.Printf("[task %s] tree tar 跳过：CODEAUDIT_STORAGE_ADDR 未配置（桶持久层不可用，卷树保留）", taskID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), treeTarUploadTimeout)
	defer cancel()

	archive, err := os.CreateTemp("", fmt.Sprintf("tree-%s-*.tar.gz", taskID))
	if err != nil {
		log.Printf("[task %s] tree tar 临时文件失败（不阻塞任务）: %v", taskID, err)
		return
	}
	tmpPath := archive.Name()
	defer os.Remove(tmpPath)
	n, err := writeTreeTarGz(archive, root)
	archive.Close()
	if err != nil {
		log.Printf("[task %s] tree tar 打包失败（不阻塞任务，卷树保留）: %v", taskID, err)
		return
	}

	fileID, err := storageUploadFile(ctx, storageAddr,
		fmt.Sprintf("trees/%s.tar.gz", taskID), "application/gzip", func() (io.Reader, error) {
			return os.Open(tmpPath)
		})
	if err != nil {
		log.Printf("[task %s] tree tar 入桶失败（不阻塞任务，卷树保留——GC 永不删除无锚点树）: %v", taskID, err)
		return
	}
	s.mu.Lock()
	cur, ok := s.tasks[taskID]
	if ok && cur.Config != nil && cur.Config["tree_tar_file_id"] == "" {
		cur.Config["tree_tar_file_id"] = fileID
		s.persistTaskLocked(cur)
	}
	s.mu.Unlock()
	log.Printf("[task %s] tree tar 已入桶：trees/%s.tar.gz（%d 文件，file_id=%s）", taskID, taskID, n, fileID)
}

// writeTreeTarGz — root → tar.gz 写入 w（排除 diffDirExcludes 与 incrementalDiffFileName）。
// maxTreeTarBytes — 单任务源码树 tar 体积上限（R62）。超限放弃入桶（任务不失败，
// A12.2 同口径），桶 SSOT 退化为上传原件兜底（基线重物化层③）。
const maxTreeTarBytes = 512 << 20

func writeTreeTarGz(w io.Writer, root string) (int, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	n := 0      // 文件计数（roundtrip 断言消费）
	total := 0  // R62: 累计字节（上限判定，勿混入 n）
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
		hdr := &tar.Header{Name: normalizeRepoPath(rel), Mode: int64(info.Mode().Perm()), Size: info.Size()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if total+int(info.Size()) > maxTreeTarBytes { // R62: 累计字节上限（防大仓库 OOM；放弃入桶+日志，任务不失败）
			return fmt.Errorf("tree tar exceeds %d bytes (skipped, R62)", maxTreeTarBytes)
		}
		f, ferr := os.Open(p)
		if ferr != nil {
			return ferr
		}
		copied, cerr := io.Copy(tw, f)
		total += int(copied)
		f.Close()
		if cerr != nil {
			return cerr
		}
		n++
		return nil
	})
	if err != nil {
		return n, err
	}
	if err := tw.Close(); err != nil {
		return n, err
	}
	if err := gz.Close(); err != nil {
		return n, err
	}
	return n, nil
}

// storageUploadFile — storage UploadFile 客户端流上传（首块带 file_path+content_type）。
func storageUploadFile(ctx context.Context, addr, filePath, contentType string, open func() (io.Reader, error)) (string, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stream, err := pb.NewStorageServiceClient(conn).UploadFile(ctx)
	if err != nil {
		return "", err
	}
	f, err := open()
	if err != nil {
		return "", err
	}
	if closer, ok := f.(io.Closer); ok {
		defer closer.Close()
	}
	first := true
	buf := make([]byte, treeTarChunkBytes)
	for {
		rn, rerr := f.Read(buf)
		if rn > 0 {
			chunk := &pb.UploadFileChunk{Data: buf[:rn]}
			if first {
				chunk.FirstChunk = true
				chunk.FilePath = filePath
				chunk.ContentType = contentType
				first = false
			}
			if serr := stream.Send(chunk); serr != nil {
				return "", serr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", err
	}
	return resp.GetFileId(), nil
}

// ---- 周期对账回收器（ADR-225 §4.10 三触发线）----

// repoCacheGC — 卷缓存回收器（nil-safe：未启用时 tick 直接跳过）。
type repoCacheGC struct {
	s         *TaskServiceImpl
	enabled   bool
	interval  time.Duration
	ttl       time.Duration
	orphanTTL time.Duration
	maxBytes  int64
}

func (g *repoCacheGC) run() {
	if g == nil || !g.enabled {
		return
	}
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for range t.C {
		g.tick()
	}
}

func (g *repoCacheGC) tick() {
	g.s.sweepTerminalAux(time.Now()) // R69: 终态任务辅助态清扫（见下）
	storageAddr := envOr("CODEAUDIT_STORAGE_ADDR", "")
	if storageAddr == "" {
		return // 无 storage 配置=直跑形态：树 tar 从未入桶，卷树是唯一副本，绝不回收
	}
	// 硬保护①：memory 档全线停用（对象在内存重启即失，删卷树=数据不可恢复）
	mode, err := storageMode(storageAddr)
	if err != nil {
		log.Printf("[repo-cache-gc] 档位探测失败，本轮跳过（不猜）: %v", err)
		return
	}
	if mode != "s3" {
		log.Printf("[repo-cache-gc] storage=%s 档，全部触发线停用（ADR-225 硬保护）", mode)
		return
	}
	entries, err := os.ReadDir(g.s.reposDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || gcProtected(name) {
			continue
		}
		dir := filepath.Join(g.s.reposDir, name)
		if strings.HasPrefix(name, "rehy-") {
			// 基线重物化 scratch：短 TTL 孤儿通道（正常路径 diff 后即清，此处兜底残骸）
			g.gcDirIfOld(dir, name, g.orphanTTL, now, "孤儿（重物化 scratch 残留）", 0)
			continue
		}
		taskID, ok := cacheDirTaskID(name)
		if !ok {
			g.gcDirIfOld(dir, name, g.orphanTTL, now, "孤儿（无法解析任务号）", 0)
			continue
		}
		g.s.mu.RLock()
		task, known := g.s.tasks[taskID]
		var terminal bool
		var updatedAt time.Time
		var tarFileID string
		if known {
			terminal = isTerminalStatus(task.GetStatus())
			updatedAt = task.GetUpdatedAt().AsTime()
			tarFileID = task.GetConfig()["tree_tar_file_id"]
		}
		g.s.mu.RUnlock()
		if !known {
			g.gcDirIfOld(dir, name, g.orphanTTL, now, "孤儿（任务记录不存在）", 0)
			continue
		}
		if !terminal {
			continue // 硬保护②：运行中/PAUSED 任务的树绝不删
		}
		// TTL 线（触发①）
		if g.ttl >= 0 && now.Sub(updatedAt) > g.ttl {
			_, _ = g.evictIfTarInBucket(dir, taskID, tarFileID, storageAddr, "TTL")
			continue
		}
	}
	// 容量线（触发②）：超限→终态候选旧→新强制驱逐
	g.enforceCapacity(storageAddr, now)
}

// evictIfTarInBucket — 删除前提=树 tar 已入桶且 HEAD 校验通过；无锚点/校验失败
// 一律保留（诚实降级：宁可占盘，不可丢唯一副本）。
func (g *repoCacheGC) evictIfTarInBucket(dir, taskID, tarFileID, storageAddr, reason string) (int64, bool) {
	if tarFileID == "" {
		return 0, false
	}
	if _, err := storageFileInfo(storageAddr, tarFileID); err != nil {
		log.Printf("[repo-cache-gc] %s 树 tar 锚点 %s 校验失败，保留卷树: %v", taskID, tarFileID, err)
		return 0, false
	}
	bytes := dirSize(dir) // R52: 驱逐前量体积——删后 dirSize 恒 0，容量水位永不到达
	if err := quarantineRemove(dir); err != nil {
		log.Printf("[repo-cache-gc] %s 删除失败（下一轮重试）: %v", taskID, err)
		return 0, false
	}
	log.Printf("[repo-cache-gc] evict task=%s reason=%s bytes=%d", taskID, reason, bytes)
	return bytes, true
}

// enforceCapacity — 容量压力线：终态且 tar 在桶的任务卷旧→新驱逐至 90% 水位。
// 无可驱逐对象（全在跑/无锚点）→ WARN（不删任何在跑任务的树——红线）。
func (g *repoCacheGC) enforceCapacity(storageAddr string, now time.Time) {
	total := dirSize(g.s.reposDir)
	if total <= g.maxBytes || g.maxBytes <= 0 {
		return
	}
	type candidate struct {
		dir, taskID, tarID string
		updatedAt          time.Time
	}
	var cands []candidate
	entries, err := os.ReadDir(g.s.reposDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || gcProtected(e.Name()) {
			continue
		}
		taskID, ok := cacheDirTaskID(e.Name())
		if !ok {
			continue
		}
		g.s.mu.RLock()
		task, known := g.s.tasks[taskID]
		g.s.mu.RUnlock()
		if !known || !isTerminalStatus(task.GetStatus()) {
			continue
		}
		if task.GetConfig()["tree_tar_file_id"] == "" {
			continue
		}
		cands = append(cands, candidate{
			dir: filepath.Join(g.s.reposDir, e.Name()), taskID: taskID,
			tarID: task.GetConfig()["tree_tar_file_id"], updatedAt: task.GetUpdatedAt().AsTime(),
		})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].updatedAt.Before(cands[j].updatedAt) })
	target := g.maxBytes * 9 / 10
	for _, c := range cands {
		if total <= target {
			break
		}
		// R52: 记账用驱逐前体积（evict 内部先量后删）——此前删后 dirSize 恒 0，
		// total 永不下降→水位 break 失效→超线即逐出全部候选
		if freed, ok := g.evictIfTarInBucket(c.dir, c.taskID, c.tarID, storageAddr, "容量"); ok {
			total -= freed
		}
	}
	if total > target {
		log.Printf("[repo-cache-gc] WARN: 卷用量 %d 超容量线 %d 且无可驱逐对象（运行中任务受保护不删）", total, g.maxBytes)
	}
}

// gcDirIfOld — 孤儿通道：mtime 超过 ttl 即清理并记账（repo 型树是唯一副本——清理前必记账）。
func (g *repoCacheGC) gcDirIfOld(dir, name string, ttl time.Duration, now time.Time, reason string, _ int) {
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if now.Sub(fi.ModTime()) <= ttl {
		return
	}
	bytes := dirSize(dir)
	if err := quarantineRemove(dir); err != nil {
		return
	}
	log.Printf("[repo-cache-gc] evict dir=%s reason=%s bytes=%d", name, reason, bytes)
}

// quarantineRemove — 先原子改名隔离（.gc-<ts>）再 rm：已打开句柄不断流（POSIX），
// 新查找 miss 后走重物化（ADR-225 §4.10 并发安全）。
func quarantineRemove(dir string) error {
	q := fmt.Sprintf("%s.gc-%d", dir, time.Now().UnixNano())
	if err := os.Rename(dir, q); err != nil {
		return err
	}
	return os.RemoveAll(q)
}

// cacheDirTaskID — 卷目录名 → 任务号（uploads-<id> 上传型 / <task_id> repo 型
// 两形态；解析不出的名字原样返回，由 s.tasks 查不到走孤儿通道兜底）。
func cacheDirTaskID(name string) (string, bool) {
	if id, ok := strings.CutPrefix(name, "uploads-"); ok && id != "" {
		return id, true
	}
	return name, true
}

// gcProtected — 回收器绝不触碰的卷内邻居（名字精确 + .gc- 中转态跳过）。
func gcProtected(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true // .gateway-cache / .gc-* 中转态 / 其他隐藏目录
	}
	for _, p := range gcProtectedNames {
		if name == p {
			return true
		}
	}
	return false
}

// isTerminalStatus — 仅真终态可回收（FAILED 除外：自动重试在途，重试链会重新
// re-prepare 同一目录，并发删除=竞态；ADR-225 §4.10 前提"终态"口径与状态机
// 终态判定一致：COMPLETED/CANCELLED/TIMEOUT/DEAD）。
func isTerminalStatus(st pb.TaskStatus) bool {
	switch st {
	case pb.TaskStatus_TASK_STATUS_COMPLETED, pb.TaskStatus_TASK_STATUS_CANCELLED,
		pb.TaskStatus_TASK_STATUS_TIMEOUT, pb.TaskStatus_TASK_STATUS_DEAD:
		return true
	}
	return false
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// storageMode / storageFileInfo — storage 只读探测（档位/锚点 HEAD）。
func storageMode(addr string) (string, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := pb.NewStorageServiceClient(conn).GetStorageMode(ctx, &pb.GetStorageModeRequest{})
	if err != nil {
		return "", err
	}
	return resp.GetMode(), nil
}

func storageFileInfo(addr, fileID string) (*pb.StoredFile, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return pb.NewStorageServiceClient(conn).GetFileInfo(ctx, &pb.GetFileInfoRequest{FileId: fileID})
}


// sweepTerminalAux — R69（2026-09-12 待办收尾）：终态且超出保留窗的任务，清扫其
// 运行期辅助态（logs——ADR-167 内存态设计、重启即忘，清扫同语义；incrementalCtx——
// diff 快照已在任务实体，上下文可弃）。防长生命周期服务无界增长。contexts 保留
// （详情页 output_refs 消费面）。幂等三表超容量整体重置（语义=重启遗忘，03 §2 重放
// 由 R68 任务侧指纹兜底）。
func (s *TaskServiceImpl) sweepTerminalAux(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	horizon := 24 * time.Hour
	for id, task := range s.tasks {
		if !isTerminalStatus(task.GetStatus()) {
			continue
		}
		ref := task.GetUpdatedAt()
		if ref == nil {
			continue
		}
		if now.Sub(ref.AsTime()) < horizon {
			continue
		}
		delete(s.logs, id)
		delete(s.incrementalCtx, id)
		delete(s.cancels, id)
	}
	if len(s.idem) > 50_000 {
		s.idem = make(map[string]*idemRecord)
	}
	if len(s.logIdem) > 100_000 {
		s.logIdem = make(map[string]string)
	}
}
