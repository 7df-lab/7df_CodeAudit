package service

// ADR-248 切片4后半锁定测试：授权目标三元组强契约校验（拒载给字段路径，D1）。

import (
	"strings"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestValidateAuthorizedTargets(t *testing.T) {
	ok := []*pb.AuthorizedTarget{
		{Host: "target.example.com", Port: 443, AllowedIps: []string{"203.0.113.7"}},
		{Host: "db.internal", Port: 3306, AllowedIps: []string{"10.0.0.0/8", "192.168.1.1"}},
	}
	if err := validateAuthorizedTargets(ok); err != nil {
		t.Fatalf("合法三元组被拒: %v", err)
	}
	if err := validateAuthorizedTargets(nil); err != nil {
		t.Fatalf("空目标（restricted 缺省态）应放行: %v", err)
	}
	cases := []struct {
		name   string
		target *pb.AuthorizedTarget
		path   string // 期望的拒载字段路径
	}{
		{"host 为 IP 字面量", &pb.AuthorizedTarget{Host: "203.0.113.7", Port: 443, AllowedIps: []string{"203.0.113.7"}}, "authorized_targets[0].host"},
		{"host 非法字符", &pb.AuthorizedTarget{Host: "bad_host!", Port: 443, AllowedIps: []string{"10.0.0.1"}}, "authorized_targets[0].host"},
		{"host 空", &pb.AuthorizedTarget{Port: 443, AllowedIps: []string{"10.0.0.1"}}, "authorized_targets[0].host"},
		{"port 零", &pb.AuthorizedTarget{Host: "a.b", AllowedIps: []string{"10.0.0.1"}}, "authorized_targets[0].port"},
		{"port 越界", &pb.AuthorizedTarget{Host: "a.b", Port: 70000, AllowedIps: []string{"10.0.0.1"}}, "authorized_targets[0].port"},
		{"allowed_ips 空", &pb.AuthorizedTarget{Host: "a.b", Port: 443}, "authorized_targets[0].allowed_ips"},
		{"allowed_ips 坏 CIDR", &pb.AuthorizedTarget{Host: "a.b", Port: 443, AllowedIps: []string{"10.0.0.300/24"}}, "authorized_targets[0].allowed_ips[0]"},
	}
	for _, c := range cases {
		err := validateAuthorizedTargets([]*pb.AuthorizedTarget{c.target})
		if err == nil || !strings.Contains(err.Error(), c.path) {
			t.Fatalf("%s: 拒载=%v（须给字段路径 %q）", c.name, err, c.path)
		}
	}
	// 第二元素路径索引
	err := validateAuthorizedTargets([]*pb.AuthorizedTarget{
		{Host: "a.b", Port: 443, AllowedIps: []string{"10.0.0.1"}},
		{Host: "c.d", Port: 0, AllowedIps: []string{"10.0.0.2"}},
	})
	if err == nil || !strings.Contains(err.Error(), "authorized_targets[1].port") {
		t.Fatalf("多元素路径索引: %v", err)
	}
}

// TestTaskOrigin_NetworkFields — origin 派生：network_profile（pack 快照键）+
// authorized_targets（ScanTask 快照）随 TaskOrigin 贯穿；均空=restricted 缺省态。
func TestTaskOrigin_NetworkFields(t *testing.T) {
	o := taskOriginOf(&pb.ScanTask{
		CreatedBy: "u1",
		Config:    map[string]string{"network_profile": "targeted"},
		AuthorizedTargets: []*pb.AuthorizedTarget{
			{Host: "t.example", Port: 443, AllowedIps: []string{"203.0.113.7"}},
		},
	})
	if o.GetNetworkProfile() != "targeted" || len(o.GetAuthorizedTargets()) != 1 || o.GetAuthorizedTargets()[0].GetHost() != "t.example" {
		t.Fatalf("origin network fields = %+v", o)
	}
	plain := taskOriginOf(&pb.ScanTask{CreatedBy: "u1"})
	if plain.GetNetworkProfile() != "" || len(plain.GetAuthorizedTargets()) != 0 {
		t.Fatalf("无 pack 无目标=restricted 缺省态, got %+v", plain)
	}
}

// TestCreateScanTask_AuthorizedTargetsSnapshot — 创建即校验+快照：坏目标 400
// （字段路径），合法目标进 ScanTask（重试不漂移的快照面）。
func TestCreateScanTask_AuthorizedTargetsSnapshot(t *testing.T) {
	s := newSvc(t)
	_, err := s.CreateScanTask(t.Context(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "at-bad"},
		ProjectId: "p1", ScanMode: pb.ScanMode_SCAN_MODE_SAST_ONLY,
		AuthorizedTargets: []*pb.AuthorizedTarget{{Host: "bad!", Port: 1, AllowedIps: []string{"10.0.0.1"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "authorized_targets[0].host") {
		t.Fatalf("坏目标须 400 给字段路径: %v", err)
	}
	task, err := s.CreateScanTask(t.Context(), &pb.CreateScanTaskRequest{
		Metadata:  &pb.RequestMetadata{RequestId: "at-ok"},
		ProjectId: "p1", ScanMode: pb.ScanMode_SCAN_MODE_SAST_ONLY,
		AuthorizedTargets: []*pb.AuthorizedTarget{{Host: "t.example", Port: 443, AllowedIps: []string{"203.0.113.7"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.GetAuthorizedTargets()) != 1 || task.GetAuthorizedTargets()[0].GetPort() != 443 {
		t.Fatalf("快照缺失: %+v", task.GetAuthorizedTargets())
	}
}
