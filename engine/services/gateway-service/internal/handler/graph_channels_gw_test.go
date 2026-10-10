// graph-ui-channels 批2 网关面回归：node_cursor 查询参数解析 +
// 通道增量合并（流式/轮询两路共用面）。服务端组装面（Get/Stream channels）的
// 契约由 runtime 仓测试覆盖；本文件锁网关纯函数。
package handler

import (
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestParseNodeCursors(t *testing.T) {
	for _, tc := range []struct {
		name string
		vals []string
		want map[string]int64
	}{
		{"空", nil, nil},
		{"单值", []string{"n001:12"}, map[string]int64{"n001": 12}},
		{"逗号多组", []string{"n001:12,n002:34"}, map[string]int64{"n001": 12, "n002": 34}},
		{"可重复参数", []string{"n001:12", "n002:34"}, map[string]int64{"n001": 12, "n002": 34}},
		{"非法跳过", []string{"n001:-1,n002:abc,n003:5"}, map[string]int64{"n003": 5}},
		{"无冒号跳过", []string{"n001"}, nil},
	} {
		got := parseNodeCursors(tc.vals)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s=%d want %d", tc.name, k, got[k], v)
			}
		}
	}
}

func TestMergeAIChannels(t *testing.T) {
	dst := &pb.GetAIInteractionLogResponse{}
	cursors := map[string]int64{}
	mergeAIChannels(dst, []*pb.AILogChannel{
		{NodeId: "n001", Chunk: []byte("ab"), NextCursor: 2, TotalBytes: 10},
		{NodeId: "n002", Chunk: []byte("x"), NextCursor: 1, TotalBytes: 1, Complete: true},
	}, cursors)
	mergeAIChannels(dst, []*pb.AILogChannel{
		{NodeId: "n001", Chunk: []byte("cd"), NextCursor: 4, TotalBytes: 10},
	}, cursors)
	if len(dst.Channels) != 2 {
		t.Fatalf("通道数 %d want 2", len(dst.Channels))
	}
	var c1 *pb.AILogChannel
	for _, c := range dst.Channels {
		if c.NodeId == "n001" {
			c1 = c
		}
	}
	if string(c1.Chunk) != "abcd" || c1.NextCursor != 4 || c1.TotalBytes != 10 {
		t.Errorf("n001 合并失常: %q next=%d", c1.Chunk, c1.NextCursor)
	}
	if !dst.Channels[1].Complete {
		t.Error("n002 complete 位丢失")
	}
	if cursors["n001"] != 4 || cursors["n002"] != 1 {
		t.Errorf("游标同步失常: %v", cursors)
	}
}

// TestMergeAIChannels_SelfMergeGuard — 轮询路形态锁（审查 P1-1 逃逸口）：
// 若有人把 mergeAIChannels 改回自合并用法（dst=ai, chans=ai.Channels），
// 本用例示范其翻倍后果——正确用法（仅累进游标）不经过本函数。
func TestMergeAIChannels_SelfMergeGuard(t *testing.T) {
	ai := &pb.GetAIInteractionLogResponse{Channels: []*pb.AILogChannel{
		{NodeId: "n001", Chunk: []byte("段"), NextCursor: 3, TotalBytes: 3},
	}}
	cursors := map[string]int64{}
	// 正确用法：轮询路只累进游标（taskwatch 轮询分支的现行实现形态）
	for _, c := range ai.GetChannels() {
		cursors[c.GetNodeId()] = c.GetNextCursor()
	}
	if cursors["n001"] != 3 {
		t.Fatalf("游标累进失常: %v", cursors)
	}
	if string(ai.Channels[0].Chunk) != "段" {
		t.Fatalf("帧内 chunk 被改写: %q", ai.Channels[0].Chunk)
	}
	// 反例锁（自合并翻倍——防回归示范，勿在产线使用）
	dup := &pb.GetAIInteractionLogResponse{Channels: ai.GetChannels()}
	mergeAIChannels(dup, dup.GetChannels(), nil)
	if len(dup.Channels[0].Chunk) != 6 {
		t.Fatalf("自合并翻倍假设失效（助手已改？请同步轮询路实现）: %q", dup.Channels[0].Chunk)
	}
}
