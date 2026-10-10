package service

// 第四轮审计 P1-1 锁定测试：CreateScanTask 的 request_id（=task_id）是客户端
// 可控串，且直接进文件系统路径（repos/<task_id> 的 clone 前 RemoveAll、
// uploads-<task_id> 解包、collectUsage 读路径）——含 ".."/分隔符即路径穿越+
// 任意目录删除；"." 单独=Join 后指向 reposDir 本身。R53/R123 同漏洞类第三处，
// 入口白名单与 graph 路径同款。网关产 gw-<hex> 不受影响（直写 gRPC 面才可达）。

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/codeaudit/proto-gen"
)

func TestCreateScanTask_RejectsPathUnsafeTaskID(t *testing.T) {
	s := newSvc(t)
	bad := []string{
		"../escape",              // 穿越父目录
		"a/b",                    // 路径分隔符（Join 后逃逸 reposDir 子目录约束）
		`a\b`,                    // 反斜杠（Windows 分隔符同罪）
		"..",                     // 白名单字符集合法但穿越段
		".",                      // Join=reposDir 本身，RemoveAll 清空整个缓存目录
		"a/../b",                 // 混合穿越
		strings.Repeat("x", 129), // 长度上限锚（>128）
	}
	for _, id := range bad {
		_, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
			Metadata:  &pb.RequestMetadata{RequestId: id},
			ProjectId: "p-x",
			ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("request_id %q 应被 InvalidArgument 拒绝，得到 %v", id, err)
		}
	}
	// 合法形状不误伤：网关产 gw-<hex> 与测试常用 req-* 均放行
	for _, id := range []string{"gw-9f8e7d6c5b4a", "req-normal-1", "task.2026_10-03"} {
		if _, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
			Metadata:  &pb.RequestMetadata{RequestId: id},
			ProjectId: "p-" + id,
			ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		}); err != nil {
			t.Fatalf("合法 request_id %q 不应被拒: %v", id, err)
		}
	}
}
