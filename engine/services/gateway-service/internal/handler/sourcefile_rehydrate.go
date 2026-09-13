// sourcefile_rehydrate.go — 任务源码树⑤流：从 storage 树 tar 重物化（ADR-225 D6/F14）。
//
// 依据: 伞仓 docs/designs/incremental-scan.md §4.10——卷树被 GC 驱逐后 source-file
// 仍须 200：按 task.config.tree_tar_file_id（task-service 树 tar 入桶锚点）下载
// trees/<task_id>.tar.gz 解包到 gateway 本地缓存（字节上限 + LRU），root_via 如实
// 标注 "tree_tar_rehydrated"。锚点缺失/下载/解包失败 → 诚实维持原 404 口径。
// 缓存目录 ReposDir/.gateway-cache 受 task-service 回收器保护（gcProtectedNames），
// 本文件自管 LRU（旧→新驱逐——tar 在桶前提下删缓存安全，下次访问重物化）。
package handler

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
	"strconv"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
)

const (
	// rehydrateCacheMaxBytes — source-file 重物化缓存字节上限（env 可覆盖）。
	rehydrateCacheMaxBytes = 2 << 30 // 2GiB
	rehydrateDialTimeout   = 30 * time.Second
	rehydrateMaxTotalBytes = 500 << 20 // 解包总量上限（与 task archive.go 同口径）
	rehydrateMaxFiles      = 100000    // 文件数上限（同口径）
)

// rehydrateCacheRoot — gateway 重物化缓存根（ ReposDir 下受保护的邻居目录）。
func rehydrateCacheRoot() string {
	if ReposDir == "" {
		return ""
	}
	return filepath.Join(ReposDir, ".gateway-cache")
}

// rehydrateFromTreeTar — ⑤流入口：缓存命中直接用（LRU 触碰 mtime）；miss 则
// 下载树 tar 解包入缓存。返回 (剥壳根, via, error)。
func (t *Transcoder) rehydrateFromTreeTar(taskID, fileID string) (string, string, error) {
	root := rehydrateCacheRoot()
	if root == "" {
		return "", "", fmt.Errorf("repos_dir 未配置，无重物化缓存位")
	}
	taskDir := filepath.Join(root, taskID)
	unpacked := filepath.Join(taskDir, "unpacked")
	// 缓存命中优先（零后端依赖：命中路径不需要 storageConn）
	if fi, err := os.Stat(unpacked); err == nil && fi.IsDir() {
		// LRU 触碰：enforceRehydrateLRU 以 <id> 父目录 mtime 为驱逐序键，
		// 命中必须同时 touch 父目录与 unpacked——只 touch 子目录会让命中永不续期
		// （实测缺陷：新缓存反被先驱逐）
		now := time.Now()
		_ = os.Chtimes(taskDir, now, now)
		_ = os.Chtimes(unpacked, now, now)
		return resolveProjectRoot(unpacked), "tree_tar_rehydrated", nil
	}
	if fileID == "" {
		return "", "", fmt.Errorf("任务无树 tar 锚点（ADR-225 之前创建或入桶失败）")
	}
	if t.storageConn == nil {
		return "", "", fmt.Errorf("storage 未接线")
	}
	dctx, cancel := context.WithTimeout(context.Background(), rehydrateDialTimeout)
	defer cancel()
	// R70（2026-09-12 待办收尾）：并发 miss 双下载互拆——解包先入临时目录，成功后
	// rename 原子就位。R79: 临时目录加唯一后缀——共享 `<id>.tmp` 在并发 miss 下，
	// 入口/收尾的 RemoveAll 会互拆对方目录，可产出"缺文件但 stat 存在"的投毒缓存树
	// （R70 tmp-rename 原子化的并发盲区）；唯一命名后"只清自己的 tmp"才真正成立。
	tmpDir := taskDir + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.sweepStaleRehydrateTmp(taskID, taskDir) // R86: 孤儿清扫须先确认任务已终态，防止清掉在途业务残留
	if err := downloadAndUnpackTree(dctx, t.storageConn, fileID, filepath.Join(tmpDir, "unpacked")); err != nil {
		_ = os.RemoveAll(tmpDir) // 半成品不留（只清自己的 tmp）
		return "", "", err
	}
	_ = os.RemoveAll(unpacked) // 同任务旧半成品（无并发方时为空操作）
	if err := os.Rename(filepath.Join(tmpDir, "unpacked"), unpacked); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	_ = os.RemoveAll(tmpDir)
	log.Printf("[source-file] %s 从树 tar 重物化（file_id=%s）", taskID, fileID)
	t.enforceRehydrateLRU()
	return resolveProjectRoot(unpacked), "tree_tar_rehydrated", nil
}


// rehydrateTmpStaleThreshold — R85: 崩溃孤儿 .tmp-* 清扫阈值（复审定值：远大于正常
// 解包时长、远小于 LRU 容量污染周期；无 07 基线，取舍见 ADR-230）。
const rehydrateTmpStaleThreshold = time.Hour

// rehydrateTmpStatusTimeout — R86: 孤儿清扫前任务状态查证的超时（清扫是 miss 路径
// 的顺手清理，不得显著加路；定值无 07 基线，取舍见 ADR-230）。
const rehydrateTmpStatusTimeout = 2 * time.Second

// sweepStaleRehydrateTmp — R85 引入、R86收紧守卫：清扫同任务超阈值未动的
// .tmp-* 孤儿须同时满足——①任务已终态（GetScanTask 查证；在途任务的残留可能属
// 正常业务，且查不到状态/服务不可达时一律保守跳过）②目录 mtime 超阈值（终态任务
// 仍可能恰有在途 miss 的新鲜 tmp，时长 guard 防误删并发在途目录）。
// best-effort，错误忽略。此前共享 .tmp 的入口 RemoveAll 曾顺带自愈崩溃残留，唯一
// 命名后孤儿计入 LRU 容量且 mtime 恒最新（永排驱逐队尾），故仍需显式扫。
func (t *Transcoder) sweepStaleRehydrateTmp(taskID, taskDir string) {
	sctx, cancel := context.WithTimeout(context.Background(), rehydrateTmpStatusTimeout)
	defer cancel()
	resp, err := pb.NewTaskServiceClient(t.taskConn).GetScanTask(sctx, &pb.GetScanTaskRequest{TaskId: taskID})
	if err != nil || !isTerminalTaskStatus(resp.GetStatus()) {
		return // 查不到/未终态：保守不扫
	}
	for _, m := range staleRehydrateTmpDirs(taskDir) {
		_ = os.RemoveAll(m)
	}
}

// isTerminalTaskStatus — R86: 终态判定（gateway 侧最小副本；口径与 task-service
// isTerminalStatus 一致：COMPLETED/FAILED/CANCELLED/TIMEOUT/DEAD，两侧不可漂移）。
func isTerminalTaskStatus(st pb.TaskStatus) bool {
	switch st {
	case pb.TaskStatus_TASK_STATUS_COMPLETED,
		pb.TaskStatus_TASK_STATUS_FAILED,
		pb.TaskStatus_TASK_STATUS_CANCELLED,
		pb.TaskStatus_TASK_STATUS_TIMEOUT,
		pb.TaskStatus_TASK_STATUS_DEAD:
		return true
	}
	return false
}

// staleRehydrateTmpDirs — R85/R86: 同任务 .tmp-* 残留中 mtime 超阈值的绝对路径。
func staleRehydrateTmpDirs(taskDir string) []string {
	matches, err := filepath.Glob(taskDir + ".tmp-*")
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && time.Since(fi.ModTime()) > rehydrateTmpStaleThreshold {
			out = append(out, m)
		}
	}
	return out
}

// downloadAndUnpackTree — DownloadFile 流 → scratch tar.gz → 解包到 dest
// （穿越/软链/总量/文件数防护同 task-service archive.go 口径；仅普通文件条目）。
func downloadAndUnpackTree(ctx context.Context, conn *grpc.ClientConn, fileID, dest string) error {
	stream, err := pb.NewStorageServiceClient(conn).DownloadFile(ctx, &pb.DownloadFileRequest{FileId: fileID})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "tree-dl-*.tar.gz")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return rerr
		}
		if _, werr := tmp.Write(chunk.GetData()); werr != nil {
			tmp.Close()
			return werr
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if fi, serr := os.Stat(tmpPath); serr != nil || fi.Size() == 0 {
		return fmt.Errorf("树 tar 对象为空: %s", fileID)
	}
	return unpackTreeTarGz(tmpPath, dest)
}

// unpackTreeTarGz — tar.gz 解包（safeJoin 穿越防护 + 总量/文件数上限；仅普通文件）。
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
			continue // 目录/软链/硬链跳过
		}
		target, jerr := safeJoinTree(dest, hdr.Name)
		if jerr != nil {
			return jerr
		}
		if total+hdr.Size > rehydrateMaxTotalBytes {
			return fmt.Errorf("解包总量超过 %dMB", rehydrateMaxTotalBytes>>20)
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
		if count > rehydrateMaxFiles {
			return fmt.Errorf("解包文件数超过 %d", rehydrateMaxFiles)
		}
	}
	if count == 0 {
		return fmt.Errorf("树 tar 内没有文件")
	}
	return nil
}

// safeJoinTree — 防路径穿越 + 拒绝软链（ADR-145 同口径）。
func safeJoinTree(root, name string) (string, error) {
	clean := filepath.Clean("/" + name)
	target := filepath.Join(root, clean)
	if !strings.HasPrefix(target, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("path traversal entry rejected: %s", name)
	}
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink entry rejected: %s", name)
	}
	return target, nil
}

// enforceRehydrateLRU — 字节上限驱逐：旧 mtime 先逐（A14.2）。驱逐只删缓存
// （tar 在桶为前提——入桶失败的任务不会有锚点、不会进缓存）。
func (t *Transcoder) enforceRehydrateLRU() {
	root := rehydrateCacheRoot()
	if root == "" {
		return
	}
	max := int64(rehydrateCacheMaxBytes)
	if v := os.Getenv("CODEAUDIT_SOURCEFILE_CACHE_MAX_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			max = n
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type item struct {
		dir   string
		mtime time.Time
		size  int64
	}
	var items []item
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, serr := os.Stat(dir)
		if serr != nil {
			continue
		}
		size := dirSizeWalk(dir)
		total += size
		items = append(items, item{dir: dir, mtime: fi.ModTime(), size: size})
	}
	if total <= max {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	for _, it := range items {
		if total <= max*9/10 {
			break
		}
		// R70（2026-09-12 待办收尾）：驱逐先改名隔离再删（对齐 task-service GC 的
		// quarantine 模式）——正在读缓存的 source-file 请求文件句柄不断流（POSIX），
		// 且并发方不会命中半删除态目录
		quarantined := it.dir + ".gc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		evicted := false
		if err := os.Rename(it.dir, quarantined); err == nil {
			go os.RemoveAll(quarantined)
			evicted = true
		} else if os.RemoveAll(it.dir) == nil {
			evicted = true
		}
		if evicted {
			total -= it.size
			log.Printf("[source-file] LRU 驱逐重物化缓存 %s（%d 字节）", filepath.Base(it.dir), it.size)
		}
	}
}

func dirSizeWalk(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
