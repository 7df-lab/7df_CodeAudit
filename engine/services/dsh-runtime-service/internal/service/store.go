// Package service — dsh-runtime 结果落盘与遍历辅助。
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// walkFn — codeExt 表在 code_analysis.go，本文件只做通用遍历。
func walkFn(root string, fn func(path string, lines int32, lang string) bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "__pycache__" || name == "node_modules" || name == ".toolchain" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		lang, ok := codeExt[ext]
		if !ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := int32(strings.Count(string(data), "\n") + 1)
		if !fn(path, lines, lang) {
			return filepath.SkipAll
		}
		return nil
	})
}

// batchCreateFindingsToResult — 09 §2 行 dsh-runtime→result：批量落盘并返回成功 ID。
// R4: 幂等键 requestID 隔离重复提交（03 §2）。
// ADR-134: 错误上抛（此前静默返回 nil，落盘丢失无人知晓）。
func batchCreateFindingsToResult(resultAddr, taskID, requestID string, findings []*pb.UnifiedFinding) ([]string, error) {
	conn, err := grpc.Dial(resultAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial result-service: %w", err)
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), cfgDurationSec("persist")) // 07 §8（ADR-137）
	defer cancel()
	resp, err := client.BatchCreateFindings(ctx, &pb.BatchCreateFindingsRequest{
		Metadata: &pb.RequestMetadata{RequestId: requestID},
		Findings: findings,
	})
	if err != nil {
		return nil, fmt.Errorf("BatchCreateFindings: %w", err)
	}
	return resp.GetFindingIds(), nil
}
