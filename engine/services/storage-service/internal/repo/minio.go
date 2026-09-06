// MinIO 文件后端（ADR-199）——FileStore + Presigner 的对象存储实现。
//
// 依据: 09 §1 bucket 口径 reports/cpg/sast-raw（按 FilePath 前缀分派，自动建桶）；
// 数据 = 对象 blob（files/<file_id>），元数据 = JSON sidecar（meta/files/<file_id>）——
// 不用 amz-meta 用户元数据承载原路径（S2S 元数据值限 ASCII，中文路径会碎）。
// 凭据纪律（ADR-115）: AK/SK 只来自环境变量，不落配置文件。
package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	blobPrefix = "files/"
	metaPrefix = "meta/files/"
)

// MinioFileStore — MinIO 对象存储实现。
type MinioFileStore struct {
	client *minio.Client
	bucket string // 默认桶（FilePath 前缀未命中已知域时兜底）
	ctx    context.Context
}

// NewMinioFileStore — endpoint 形如 host:port；bucket 缺失自动创建（09 §1 三桶 + 兜底桶）。
func NewMinioFileStore(endpoint, accessKey, secretKey, bucket string, secure bool) (*MinioFileStore, error) {
	cli, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	m := &MinioFileStore{client: cli, bucket: bucket, ctx: context.Background()}
	for _, b := range []string{bucket, "reports", "cpg", "sast-raw", "uploads"} {
		exists, err := cli.BucketExists(m.ctx, b)
		if err != nil {
			return nil, fmt.Errorf("minio BucketExists %s: %w", b, err)
		}
		if !exists {
			if err := cli.MakeBucket(m.ctx, b, minio.MakeBucketOptions{}); err != nil {
				return nil, fmt.Errorf("minio MakeBucket %s: %w", b, err)
			}
		}
	}
	return m, nil
}

// bucketFor — 09 §1 域前缀 → bucket（reports/… → reports；cpg/… → cpg；sast-raw/… →
// sast-raw；其余 → 兜底桶）。
func (m *MinioFileStore) bucketFor(filePath string) string {
	switch {
	case len(filePath) >= 8 && filePath[:8] == "reports/":
		return "reports"
	case len(filePath) >= 4 && filePath[:4] == "cpg/":
		return "cpg"
	case len(filePath) >= 9 && filePath[:9] == "sast-raw/":
		return "sast-raw"
	case len(filePath) >= 8 && filePath[:8] == "uploads/":
		return "uploads"
	}
	return m.bucket
}

func (m *MinioFileStore) blobKey(fileID string) string { return blobPrefix + fileID }
func (m *MinioFileStore) metaKey(fileID string) string { return metaPrefix + fileID }

// SaveFile — 元数据 JSON sidecar + 数据 blob 双对象写入。
// 索引纪律（ADR-200 修复）: meta sidecar 恒落默认桶（file_id 的唯一寻址锚点，
// 否则按 FilePath 分桶后元数据不可寻址）；数据 blob 按域桶分派。
// 签名与 MemoryStore 对齐（无 error 返回）：不可恢复错误 panic 快速失败，
// 由 gRPC recover 语义兜底（上层不得无声丢数据）。
func (m *MinioFileStore) SaveFile(file *v1.StoredFile, data []byte) {
	if file.CreatedAt == nil {
		file.CreatedAt = timestamppb.Now()
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	meta, _ := json.Marshal(file)
	if _, err := m.client.PutObject(ctx, m.bucket, m.metaKey(file.FileId),
		bytes.NewReader(meta), int64(len(meta)), minio.PutObjectOptions{ContentType: "application/json"}); err != nil {
		panic(fmt.Sprintf("minio SaveFile meta %s: %v", file.FileId, err))
	}
	if _, err := m.client.PutObject(ctx, m.bucketFor(file.FilePath), m.blobKey(file.FileId),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: file.ContentType}); err != nil {
		panic(fmt.Sprintf("minio SaveFile blob %s: %v", file.FileId, err))
	}
}

// GetFile — 读元数据 sidecar（恒默认桶）。
func (m *MinioFileStore) GetFile(fileID string) (*v1.StoredFile, bool) {
	obj, err := m.client.GetObject(m.ctx, m.bucket, m.metaKey(fileID), minio.GetObjectOptions{})
	if err != nil {
		return nil, false
	}
	defer obj.Close()
	var f v1.StoredFile
	if err := json.NewDecoder(obj).Decode(&f); err != nil {
		return nil, false
	}
	return &f, true
}

// GetFileData — 按 meta 的 FilePath 定位域桶读 blob。
func (m *MinioFileStore) GetFileData(fileID string) ([]byte, bool) {
	f, ok := m.GetFile(fileID)
	if !ok {
		return nil, false
	}
	obj, err := m.client.GetObject(m.ctx, m.bucketFor(f.FilePath), m.blobKey(fileID), minio.GetObjectOptions{})
	if err != nil {
		return nil, false
	}
	defer obj.Close()
	buf, err := io.ReadAll(obj)
	if err != nil {
		return nil, false
	}
	return buf, true
}

// DeleteFile — 双对象删除（blob 按域桶、meta 按默认桶；幂等，404 视为已删）。
func (m *MinioFileStore) DeleteFile(fileID string) {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	if f, ok := m.GetFile(fileID); ok {
		_ = m.client.RemoveObject(ctx, m.bucketFor(f.FilePath), m.blobKey(fileID), minio.RemoveObjectOptions{})
	}
	_ = m.client.RemoveObject(ctx, m.bucket, m.metaKey(fileID), minio.RemoveObjectOptions{})
}

// ListFiles — 遍历 meta sidecar 按 FilePath 前缀过滤。实验室规模直读全量；
// 规模化后元数据索引应迁 Redis（ADR-199 边界注记）。
func (m *MinioFileStore) ListFiles(prefix string) []*v1.StoredFile {
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
	defer cancel()
	var out []*v1.StoredFile
	for obj := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{Prefix: metaPrefix, Recursive: true}) {
		if obj.Err != nil {
			continue
		}
		o, err := m.client.GetObject(ctx, m.bucket, obj.Key, minio.GetObjectOptions{})
		if err != nil {
			continue
		}
		var f v1.StoredFile
		err = json.NewDecoder(o).Decode(&f)
		o.Close()
		if err != nil {
			continue
		}
		if prefix == "" || (len(f.FilePath) >= len(prefix) && f.FilePath[:len(prefix)] == prefix) {
			out = append(out, &f)
		}
	}
	return out
}

// PresignURL — 预签名（URL_OP_UPLOAD→Presign PUT / URL_OP_DOWNLOAD→PresignedGetObject）。
// URL host = endpoint 本身（本部署 MinIO 发布在 LAN，前端/服务同网段可达）。
func (m *MinioFileStore) PresignURL(filePath string, operation v1.GetPresignedUrlRequest_UrlOp, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute // proto 缺省口径：短时效直传/直下
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	bucket := m.bucketFor(filePath)
	var (
		url *url.URL
		err error
	)
	if operation == v1.GetPresignedUrlRequest_URL_OP_UPLOAD {
		url, err = m.client.Presign(ctx, "PUT", bucket, filePath, ttl, nil)
	} else {
		url, err = m.client.PresignedGetObject(ctx, bucket, filePath, ttl, nil)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("minio presign: %w", err)
	}
	return url.String(), time.Now().Add(ttl), nil
}
