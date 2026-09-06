// Package repo — 存储后端抽象（ADR-199）。
//
// 依据: 09 §1 storage-service = MinIO(文件) + Redis(通知/幂等) + Kafka(消费)；
// 07 §10 降级纪律——memory 后端保留（E2E/无中间件环境，重启即丢须如实标注），
// 生产口径 = s3(MinIO) + redis（CODEAUDIT_STORE=s3，env 驱动，同 result-service 模式）。
package repo

import (
	"time"

	v1 "github.com/codeaudit/proto-gen"
)

// FileStore — 文件元数据+数据的读写抽象。MemoryStore 与 MinioFileStore 均实现。
// （09 §1 bucket 口径: reports / cpg / sast-raw，由实现按 FilePath 前缀分派。）
type FileStore interface {
	SaveFile(file *v1.StoredFile, data []byte)
	GetFile(fileID string) (*v1.StoredFile, bool)
	GetFileData(fileID string) ([]byte, bool)
	DeleteFile(fileID string)
	ListFiles(prefix string) []*v1.StoredFile
}

// Presigner — 预签名 URL 能力（仅 MinIO 后端具备；memory 后端诚实返回 Unimplemented）。
type Presigner interface {
	// PresignURL 依据: proto GetPresignedUrlRequest{file_path, operation, ttl_seconds}。
	PresignURL(filePath string, operation v1.GetPresignedUrlRequest_UrlOp, ttl time.Duration) (url string, expiresAt time.Time, err error)
}

// NotificationStore — 通知持久化抽象。MemoryStore 与 RedisNotificationStore 均实现。
type NotificationStore interface {
	SaveNotification(n *v1.Notification)
	GetNotification(id string) (*v1.Notification, bool)
	MarkRead(id string) (*v1.Notification, bool)
	ListNotifications(userID string, unreadOnly bool) []*v1.Notification
}

// IdempotencyStore — 写接口幂等键存取（03 §2/R4；生产=Redis，重启后仍防重放）。
type IdempotencyStore interface {
	CheckIdempotency(requestID, bodyHash string) (*IdempotencyEntry, bool, error)
	SetIdempotency(requestID, bodyHash string, response interface{})
}

// 编译期断言：MemoryStore 继续满足全部后端接口（07 §10 降级档不缩水）。
var (
	_ FileStore         = (*MemoryStore)(nil)
	_ NotificationStore = (*MemoryStore)(nil)
	_ IdempotencyStore  = (*MemoryStore)(nil)
)
