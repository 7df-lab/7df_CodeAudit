// storage_archive.go — result-service → storage 的通用归档通道（ADR-200 补遗）。
//
// 依据: 09 §2 行 result → storage UploadFile；报告归档（ADR-199）与 findings
// 导出（ADR-200 补遗）共用。失败语义: 返回 error，由调用方如实降级
// （报告本体在 PG / 导出回落 file://，不阻塞主链路）。
package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/codeaudit/proto-gen"
)

// uploadToStorage — 把 bytes 经 storage UploadFile 客户端流归档为 objPath 对象
// （首块携带路径/类型）。返回可引用的对象 URI（minio://<bucket>/<path>）。
// bucket 约定由 storage 侧按路径前缀分派（reports/ cpg/ sast-raw/ uploads/ exports/…）。
func uploadToStorage(ctx context.Context, storageAddr, objPath, contentType string, data []byte) (string, error) {
	if storageAddr == "" {
		return "", fmt.Errorf("storage addr not configured")
	}
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(storageAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("dial storage-service: %w", err)
	}
	defer conn.Close()
	stream, err := pb.NewStorageServiceClient(conn).UploadFile(dctx)
	if err != nil {
		return "", fmt.Errorf("open stream: %w", err)
	}
	const chunkSize = 64 << 10
	for begin, first := 0, true; ; {
		end := begin + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := &pb.UploadFileChunk{Data: data[begin:end]}
		if first {
			chunk.FirstChunk = true
			chunk.FilePath = objPath
			chunk.ContentType = contentType
			first = false
		}
		if serr := stream.Send(chunk); serr != nil {
			return "", fmt.Errorf("send chunk: %w", serr)
		}
		if end == len(data) {
			break
		}
		begin = end
	}
	stored, err := stream.CloseAndRecv()
	if err != nil {
		return "", fmt.Errorf("close: %w", err)
	}
	if stored.GetFileId() == "" {
		return "", fmt.Errorf("storage returned empty file_id")
	}
	return "minio://" + stored.GetFilePath(), nil
}
