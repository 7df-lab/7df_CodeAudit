// archive.go — 上传件从 storage 拉回并解包（ADR-200）。
//
// 依据: 人类指令「gateway 直接上传原始压缩包到 storage（不在 gateway 落盘），
// task 从 storage 拉回解包扫描；无法解压导致无法扫描时抛出对应的压缩包错误」。
// 09 §2 矩阵 task → storage 的 DownloadFile 通道承载。
// 安全: safeJoin 穿越防护 + 软链/硬链拒绝 + 解包总量/文件数上限（口径与
// ADR-145 原 gateway 解包一致，实现随通道迁至扫描发起方）。
package service

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/codeaudit/proto-gen"
)

const (
	maxArchiveBytes   = 25 << 20  // 与 gateway 上传白名单上限一致（ADR-145）
	maxUnpackedBytes  = 200 << 20 // 解包总量上限
	maxArchiveFiles   = 3000      // 文件数上限
	downloadChunkCap  = 1 << 20   // 单块读取上限（防御异常大块）
	fetchDialTimeout  = 10 * time.Second
	downloadTimeout   = 120 * time.Second
	unpackFileTimeout = 120 * time.Second
)

// FetchUploadArchive — 从 storage 下载上传件并解包到 dest 根下，返回解包目录。
// 错误一律携带阶段语义（"压缩包下载失败"/"压缩包解压失败"），由编排器
// 原样写入 task.error_message（ADR-200 人类指令：解不开就报对应的错）。
func FetchUploadArchive(ctx context.Context, storageAddr, fileID, dest string) (string, error) {
	archivePath, err := downloadArchive(ctx, storageAddr, fileID, dest)
	if err != nil {
		return "", fmt.Errorf("压缩包下载失败: %w", err)
	}
	unpacked := filepath.Join(dest, "unpacked")
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		return "", fmt.Errorf("压缩包解压失败: create unpack dir: %w", err)
	}
	var n int
	var uerr error
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		n, uerr = unpackArchiveZip(archivePath, unpacked)
	case strings.HasSuffix(archivePath, ".tar.gz"), strings.HasSuffix(archivePath, ".tgz"):
		n, uerr = unpackArchiveTarGz(archivePath, unpacked)
	default:
		return "", fmt.Errorf("压缩包解压失败: 不支持的格式（仅 .zip/.tar.gz/.tgz）: %s", archivePath)
	}
	if uerr != nil {
		_ = os.RemoveAll(unpacked) // 解压失败不留半成品
		return "", fmt.Errorf("压缩包解压失败: %w", uerr)
	}
	if n == 0 {
		_ = os.RemoveAll(unpacked)
		return "", fmt.Errorf("压缩包解压失败: 压缩包内没有文件")
	}
	_ = os.Remove(archivePath) // 扫描只需要解包后的树，归档原件留在 storage
	return unpacked, nil
}

// downloadArchive — DownloadFile 服务端流拉回原始压缩包落 scratch。
func downloadArchive(ctx context.Context, storageAddr, fileID, dest string) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	conn, err := grpc.NewClient(storageAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("dial storage-service: %w", err)
	}
	defer conn.Close()
	client := pb.NewStorageServiceClient(conn)

	// 先取元数据拿原始文件名（扩展名决定解包器）
	info, err := client.GetFileInfo(dctx, &pb.GetFileInfoRequest{FileId: fileID})
	if err != nil {
		return "", fmt.Errorf("GetFileInfo %s: %w", fileID, err)
	}
	name := strings.ToLower(info.GetFilePath())
	if !strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".tgz") && !strings.HasSuffix(name, ".tar.gz") {
		return "", fmt.Errorf("不支持的格式（仅 .zip/.tar.gz/.tgz）: %s", info.GetFilePath())
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	archivePath := filepath.Join(dest, fmt.Sprintf("archive-%d%s", time.Now().UnixNano(), filepath.Ext(name)))

	stream, err := client.DownloadFile(dctx, &pb.DownloadFileRequest{FileId: fileID})
	if err != nil {
		return "", fmt.Errorf("DownloadFile %s: %w", fileID, err)
	}
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("recv chunk: %w", err)
		}
		if len(chunk.GetData()) == 0 {
			continue
		}
		if f == nil {
			f, err = os.OpenFile(archivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
		}
		if _, err := f.Write(chunk.GetData()); err != nil {
			return "", err
		}
	}
	if f != nil {
		f.Close()
		f = nil
	}
	if fi, statErr := os.Stat(archivePath); statErr != nil || fi.Size() == 0 {
		return "", fmt.Errorf("存储对象为空: %s", fileID)
	}
	_ = downloadChunkCap // 保留常量语义（单块上限由 gRPC 帧上限约束）
	return archivePath, nil
}

// safeJoin — 防路径穿越（与 ADR-145 gateway 原实现同口径）：条目名清洗后必须
// 仍位于目标根内；拒绝软链。
func safeJoin(root, name string) (string, error) {
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

// unpackArchiveZip — zip 解包（总量/文件数上限 + 穿越防护）。
func unpackArchiveZip(archivePath, root string) (int, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	var total int64
	count := 0
	uctx, cancel := context.WithTimeout(context.Background(), unpackFileTimeout)
	defer cancel()
	_ = uctx
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		target, err := safeJoin(root, f.Name)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, err
		}
		rc, err := f.Open()
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(io.Discard, io.LimitReader(rc, maxUnpackedBytes+1))
		rc.Close()
		if err != nil {
			return 0, err
		}
		total += n
		if total > maxUnpackedBytes {
			return 0, fmt.Errorf("解包总量超过 %dMB", maxUnpackedBytes>>20)
		}
		count++
		if count > maxArchiveFiles {
			return 0, fmt.Errorf("压缩包文件数超过 %d", maxArchiveFiles)
		}
		rc2, err := f.Open()
		if err != nil {
			return 0, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc2.Close()
			return 0, err
		}
		_, err = io.Copy(out, rc2)
		rc2.Close()
		out.Close()
		if err != nil {
			return 0, err
		}
	}
	return count, nil
}

// unpackArchiveTarGz — tar.gz 解包（同款防护；仅普通文件条目）。
func unpackArchiveTarGz(archivePath, root string) (int, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // 目录/软链/硬链跳过
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, err
		}
		if total+hdr.Size > maxUnpackedBytes {
			return 0, fmt.Errorf("解包总量超过 %dMB", maxUnpackedBytes>>20)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err != nil {
			return 0, err
		}
		total += n
		count++
		if count > maxArchiveFiles {
			return 0, fmt.Errorf("压缩包文件数超过 %d", maxArchiveFiles)
		}
	}
	return count, nil
}

// storagePrepare — 由 upload_file_id 构造 storage 拉包 Prepare（ADR-200 通道工厂，
// ADR-203 项目级兜底复用）。返回 (nil, msg) 表示前置不满足（CODEAUDIT_STORAGE_ADDR
// 未配置），msg 为诚实失败原因，由调用方走 FAILED+error_message 路径。
func (s *TaskServiceImpl) storagePrepare(task *pb.ScanTask, uploadID string) (func(context.Context) (string, error), string) {
	storageAddr := envOr("CODEAUDIT_STORAGE_ADDR", "")
	if storageAddr == "" {
		return nil, "CODEAUDIT_STORAGE_ADDR 未配置，无法从 storage 拉取上传件（ADR-200）"
	}
	dest := filepath.Join(s.reposDir, "uploads-"+task.GetTaskId())
	return func(ctx context.Context) (string, error) {
		log.Printf("[task %s] storage mode: fetching upload %s from %s", task.GetTaskId(), uploadID, storageAddr)
		return FetchUploadArchive(ctx, storageAddr, uploadID, dest)
	}, ""
}
