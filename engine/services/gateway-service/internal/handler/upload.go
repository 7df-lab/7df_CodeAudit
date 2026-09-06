// Package handler — 代码压缩包上传（ADR-200 重构）。
//
// 人类指令口径：原始压缩包**经 gateway 直传 storage（MinIO），gateway 不落盘**。
// 此前形态（ADR-145）gateway 解包落本地盘、以目录路径为引用——单机权宜，已废弃；
// 扫描时的拉取与解包移至 task-service（archive.go，解压失败抛压缩包错误）。
// 安全: 登录后可用（JWT 链）；扩展名白名单；25MB 上限（MaxBytesReader+计数双保险）。
// 生产多节点就绪: 对象在 MinIO，gateway/task 可异机。
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

const uploadMaxBytes = 25 << 20 // 压缩包上限 25MB（ADR-145 口径沿用）

// UploadsDir — 仅 source-file 端点解析历史上传件时使用（ADR-195 遗留读路径）；
// ADR-200 起上传不再写盘，新任务目录由 task 从 storage 拉取解包产生。
var UploadsDir string

// UploadArchive — POST /v1/uploads/archive (multipart, field=file)。
// 流式管道: multipart part → storage UploadFile 客户端流（首块携带 file_path/
// content_type），gateway 全程零磁盘写入。对象路径 uploads/<upload_id><ext> →
// uploads 桶（ADR-199/200）。
// 返回: {"upload_id","file_id","file_path","size_bytes"} — file_id 供任务创建时
// 放入 config.upload_file_id（task 从 storage 拉回解包）。
func (t *Transcoder) UploadArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes)
	mr, err := r.MultipartReader() // 流式解析：不产生 gateway 侧临时文件
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form (max 25MB): "+err.Error())
		return
	}

	var filePart io.Reader
	var filename string
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			writeError(w, http.StatusBadRequest, "multipart field 'file' is required")
			return
		}
		if perr != nil {
			writeError(w, http.StatusBadRequest, "read multipart: "+perr.Error())
			return
		}
		if part.FormName() == "file" {
			filePart, filename = part, part.FileName()
			break
		}
	}

	name := strings.ToLower(filename)
	var ext, contentType string
	switch {
	case strings.HasSuffix(name, ".tar.gz"):
		ext, contentType = ".tar.gz", "application/gzip"
	case strings.HasSuffix(name, ".tgz"):
		ext, contentType = ".tgz", "application/gzip"
	case strings.HasSuffix(name, ".zip"):
		ext, contentType = ".zip", "application/zip"
	default:
		writeError(w, http.StatusBadRequest, "only .zip / .tar.gz / .tgz archives are accepted")
		return
	}

	uploadID := "up-" + newRequestID()[3:] // 复用网关随机 id（去前缀）
	objPath := "uploads/" + uploadID + ext
	t.streamUploadToStorage(w, r, uploadID, objPath, contentType, filePart)
}

// streamUploadToStorage — 管道转发（64KiB 分块，首块带路径/类型；25MB 双保险计数）。
func (t *Transcoder) streamUploadToStorage(w http.ResponseWriter, r *http.Request,
	uploadID, objPath, contentType string, src io.Reader) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	client := pb.NewStorageServiceClient(t.storageConn)
	stream, err := client.UploadFile(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "storage upload stream: "+err.Error())
		return
	}

	const chunkSize = 64 << 10
	buf := make([]byte, chunkSize)
	var total int64
	first := true
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := &pb.UploadFileChunk{Data: buf[:n]}
			if first {
				chunk.FirstChunk = true
				chunk.FilePath = objPath
				chunk.ContentType = contentType
				first = false
			}
			if serr := stream.Send(chunk); serr != nil {
				writeError(w, http.StatusBadGateway, "storage send: "+serr.Error())
				return
			}
			total += int64(n)
			if total > uploadMaxBytes {
				_ = stream.CloseSend()
				writeError(w, http.StatusBadRequest, "archive exceeds 25MB")
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			writeError(w, http.StatusBadRequest, "read upload: "+rerr.Error())
			return
		}
		if n == 0 {
			break
		}
	}
	stored, err := stream.CloseAndRecv()
	if err != nil {
		writeError(w, http.StatusBadGateway, "storage close: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"upload_id":  stored.GetFileId(),
		"file_id":    stored.GetFileId(),
		"file_path":  stored.GetFilePath(),
		"size_bytes": stored.GetSizeBytes(),
	})
}
