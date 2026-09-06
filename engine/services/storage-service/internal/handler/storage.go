// Package handler implements gRPC server handlers for storage-service.
// StorageServiceServer (codeaudit_common.proto L1066-L1073):
//
//	6 RPCs: UploadFile, DownloadFile, GetPresignedUrl, GetFileInfo, DeleteFile, ListFiles
package handler

import (
	"context"
	"log"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// StorageHandler implements v1.StorageServiceServer.
type StorageHandler struct {
	v1.UnimplementedStorageServiceServer
	svc *service.StorageSvc
}

// NewStorageHandler creates a new StorageHandler.
func NewStorageHandler(svc *service.StorageSvc) *StorageHandler {
	return &StorageHandler{svc: svc}
}

// UploadFile implements client-streaming RPC: UploadFile(stream UploadFileChunk) → StoredFile.
// The client sends a stream of chunks; the server assembles them into a single StoredFile.
// Chunk protocol (codeaudit_common.proto L1448):
//   - first_chunk: carries file_path and content_type
//   - subsequent chunks: carry only data
func (h *StorageHandler) UploadFile(stream grpc.ClientStreamingServer[v1.UploadFileChunk, v1.StoredFile]) error {
	file, data, err := h.svc.AssembleChunks(func() (*v1.UploadFileChunk, error) {
		return stream.Recv()
	})
	if err != nil {
		return err
	}

	h.svc.SaveFile(file, data)
	log.Printf("[handler] UploadFile: stored %s (%d bytes)", file.FileId, file.SizeBytes)
	return stream.SendAndClose(file)
}

// DownloadFile implements server-streaming RPC: DownloadFile(DownloadFileRequest) → stream DownloadFileChunk.
// Streams back the stored file data in 64KB chunks.
func (h *StorageHandler) DownloadFile(req *v1.DownloadFileRequest, stream grpc.ServerStreamingServer[v1.DownloadFileChunk]) error {
	if req.FileId == "" {
		return status.Error(codes.InvalidArgument, "file_id is required")
	}

	data, err := h.svc.GetFileData(req.FileId)
	if err != nil {
		return err
	}

	// Stream in 64KB chunks
	const chunkSize = 64 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := &v1.DownloadFileChunk{Data: data[offset:end]}
		if err := stream.Send(chunk); err != nil {
			return status.Errorf(codes.Internal, "failed to send chunk: %v", err)
		}
	}

	log.Printf("[handler] DownloadFile: streamed %s (%d bytes)", req.FileId, len(data))
	return nil
}

// GetPresignedUrl implements unary RPC for frontend direct upload (read RPC).
// Returns a placeholder presigned URL; real MinIO integration noted in comments (09 §1).
func (h *StorageHandler) GetPresignedUrl(ctx context.Context, req *v1.GetPresignedUrlRequest) (*v1.GetPresignedUrlResponse, error) {
	if req.FilePath == "" {
		return nil, status.Error(codes.InvalidArgument, "file_path is required")
	}

	url, expires, err := h.svc.GetPresignedURL(req.FilePath, req.Operation, req.TtlSeconds)
	if err != nil {
		return nil, err // 诚实降级: MinIO 未接入→Unimplemented（不返回假URL）
	}
	return &v1.GetPresignedUrlResponse{
		Url:       url,
		ExpiresAt: expires,
	}, nil
}

// GetFileInfo implements unary RPC (read RPC): returns metadata for a stored file.
func (h *StorageHandler) GetFileInfo(ctx context.Context, req *v1.GetFileInfoRequest) (*v1.StoredFile, error) {
	if req.FileId == "" {
		return nil, status.Error(codes.InvalidArgument, "file_id is required")
	}
	return h.svc.GetFile(req.FileId)
}

// DeleteFile implements unary RPC (write RPC, no RequestMetadata).
// Deletes a file from storage (09 §1: removes from in-memory store / MinIO object).
func (h *StorageHandler) DeleteFile(ctx context.Context, req *v1.DeleteFileRequest) (*emptypb.Empty, error) {
	if req.FileId == "" {
		return nil, status.Error(codes.InvalidArgument, "file_id is required")
	}
	if err := h.svc.DeleteFile(req.FileId); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// ListFiles implements unary RPC (read RPC): returns files matching the given prefix.
func (h *StorageHandler) ListFiles(ctx context.Context, req *v1.ListFilesRequest) (*v1.ListFilesResponse, error) {
	files := h.svc.ListFiles(req.Prefix)
	// Simple pagination: return all results (production would apply cursor-based pagination per 03 §7)
	return &v1.ListFilesResponse{
		Files: files,
	}, nil
}

// Ensure interface compliance at compile time.
var _ v1.StorageServiceServer = (*StorageHandler)(nil)
