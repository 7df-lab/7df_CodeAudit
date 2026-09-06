// Package service implements business logic for storage-service.
// Storage operations integrate with MinIO (09 §1):
//   - Buckets: reports, cpg, sast-raw
//   - UploadFile: assemble streamed chunks into a single StoredFile.
//   - DownloadFile: stream back stored file data.
//   - GetPresignedUrl: generate a MinIO presigned URL for frontend direct upload.
package service

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StorageSvc provides business-level operations for file storage.
// 依赖 FileStore 抽象（ADR-199）：memory（07 §10 降级档）/ MinIO（生产档）可切换。
type StorageSvc struct {
	store     repo.FileStore
	presigner repo.Presigner // nil=memory 档（GetPresignedUrl 诚实 Unimplemented）
}

// NewStorageSvc creates a new StorageSvc backed by the given FileStore.
func NewStorageSvc(store repo.FileStore) *StorageSvc {
	return &StorageSvc{store: store}
}

// SetPresigner — 注入预签名能力（MinIO 档专用；ADR-199）。
func (s *StorageSvc) SetPresigner(p repo.Presigner) { s.presigner = p }

// AssembleChunks reads chunks from the provided reader function until EOF and
// returns the assembled StoredFile plus the raw bytes.
// Chunk protocol (codeaudit_common.proto L1448):
//   - UploadFileChunk { data, file_path, content_type, first_chunk }
//   - first_chunk carries file_path and content_type; subsequent chunks carry only data.
//
// MinIO integration note (09 §1): in production, each chunk would be written to
// the object directly via MinIO's PutObject streaming API.
func (s *StorageSvc) AssembleChunks(recv func() (*v1.UploadFileChunk, error)) (*v1.StoredFile, []byte, error) {
	var (
		buf         []byte
		filePath    string
		contentType string
	)

	for {
		chunk, err := recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, status.Errorf(codes.Internal, "failed to receive chunk: %v", err)
		}

		if chunk.FirstChunk {
			filePath = chunk.FilePath
			contentType = chunk.ContentType
		}
		buf = append(buf, chunk.Data...)
	}

	if filePath == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "first chunk must set file_path")
	}

	checksum := sha256Hex(buf)
	fileID := repo.GenerateID("file")

	file := &v1.StoredFile{
		FileId:         fileID,
		FilePath:       filePath,
		SizeBytes:      int64(len(buf)),
		ContentType:    contentType,
		ChecksumSha256: checksum,
		CreatedAt:      timestamppb.Now(),
	}

	log.Printf("[storage] assembled file %s: path=%s size=%d", fileID, filePath, len(buf))
	return file, buf, nil
}

// SaveFile persists a file in the store.
func (s *StorageSvc) SaveFile(file *v1.StoredFile, data []byte) {
	s.store.SaveFile(file, data)
}

// GetFile retrieves a StoredFile by id.
func (s *StorageSvc) GetFile(fileID string) (*v1.StoredFile, error) {
	f, ok := s.store.GetFile(fileID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "file %s not found", fileID)
	}
	return f, nil
}

// GetFileData retrieves the raw bytes for a file.
func (s *StorageSvc) GetFileData(fileID string) ([]byte, error) {
	d, ok := s.store.GetFileData(fileID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "file %s not found", fileID)
	}
	return d, nil
}

// DeleteFile removes a file from the store.
func (s *StorageSvc) DeleteFile(fileID string) error {
	if _, ok := s.store.GetFile(fileID); !ok {
		return status.Errorf(codes.NotFound, "file %s not found", fileID)
	}
	s.store.DeleteFile(fileID)
	log.Printf("[storage] deleted file %s", fileID)
	return nil
}

// ListFiles returns files matching the given prefix.
// MinIO prefix corresponds to object key prefixes within buckets
// (09 §1: reports/, cpg/, sast-raw/).
func (s *StorageSvc) ListFiles(prefix string) []*v1.StoredFile {
	return s.store.ListFiles(prefix)
}

// GetPresignedURL generates a presigned URL for frontend direct upload/download.
// MinIO 未接入时的诚实语义: 返回 Unimplemented 而非编造一个必然 404 的假 URL。
// (2026-08-27 编造审计: 旧实现拼 http://minio:9000/... 假地址冒充可用链接)
// MinIO 接入后: 调用 PresignedPutObject/PresignedGetObject（09 §1: reports, cpg, sast-raw）。
func (s *StorageSvc) GetPresignedURL(filePath string, operation v1.GetPresignedUrlRequest_UrlOp, ttlSeconds int64) (string, *timestamppb.Timestamp, error) {
	if s.presigner == nil {
		// memory 档（07 §10）诚实语义: Unimplemented 而非编造必然 404 的假 URL（2026-08-27 编造审计）
		return "", nil, status.Error(codes.Unimplemented,
			"presigned URL requires MinIO backend (09 §1); memory store mode has no presigner")
	}
	url, expiresAt, err := s.presigner.PresignURL(filePath, operation, time.Duration(ttlSeconds)*time.Second)
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "presign: %v", err)
	}
	return url, timestamppb.New(expiresAt), nil
}

// sha256Hex returns the hex-encoded SHA-256 digest of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}
