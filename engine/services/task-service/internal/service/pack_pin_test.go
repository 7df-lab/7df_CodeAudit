package service

// 任务创建钉修订版测试（切片 2 DoD"任务钉死修订版"语义的服务面闭环）。
// 依据: 主仓库 docs/designs/platform-slice2-pack-schema.md §3（task.config 约定键）/
// §4（审计回填双轨：不带 pack 键=缺省负载面零改动）；ADR-245。

import (
	"context"
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestCreateScanTask_PinsPackRevision(t *testing.T) {
	s := newSvc(t)
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "pin-1"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"pack": "audit-sample"},
	})
	if err != nil {
		t.Fatalf("create with pack: %v", err)
	}
	if got := task.GetConfig()["pack_revision_id"]; !strings.HasPrefix(got, "audit-sample@") {
		t.Fatalf("pack_revision_id = %q, want audit-sample@<n>", got)
	}
	if len(task.GetConfig()["pack_spec_hash"]) != 64 {
		t.Fatalf("pack_spec_hash missing/short: %q", task.GetConfig()["pack_spec_hash"])
	}
	// ADR-248：网络分型档位随修订钉扎（audit-sample=restricted——审计负载缺省档）
	if got := task.GetConfig()["network_profile"]; got != "restricted" {
		t.Fatalf("network_profile = %q, want restricted（pack spec 快照）", got)
	}
}

// 任务不带 pack 键：零改动兼容（双轨过渡面，行为与回填前一致）。
func TestCreateScanTask_NoPackKeyUnchanged(t *testing.T) {
	s := newSvc(t)
	task, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "pin-2"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.GetConfig()["pack_revision_id"] != "" {
		t.Fatalf("legacy task must not carry pack pin, got %q", task.GetConfig()["pack_revision_id"])
	}
}

// 未知 pack：创建即 4xx（fail-fast，不静默回落缺省负载——设计 §3 语义）。
func TestCreateScanTask_UnknownPackFails(t *testing.T) {
	s := newSvc(t)
	_, err := s.CreateScanTask(context.Background(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "pin-3"},
		ProjectId: "p1",
		ScanMode:  pb.ScanMode_SCAN_MODE_SAST_ONLY,
		Config:    map[string]string{"pack": "no-such-pack"},
	})
	if err == nil || !strings.Contains(err.Error(), "no-such-pack") {
		t.Fatalf("unknown pack must fail loud naming it, got %v", err)
	}
}
