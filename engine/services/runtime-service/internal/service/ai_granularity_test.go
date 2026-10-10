package service

// b4（platform-acp-bridge-v1 §4）：AILogChannel/主面 granularity 契约字段化——
// 真源=方言（协议固有属性）；TODO#13 同批：条目淘汰后 Get 的通道盘回落
// （原 entry==nil 分支早退绕过 channelDeltas 的盘回落=旧图任务弹层通道回空）。

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestGranularityOf_DialectMapping(t *testing.T) {
	cases := []struct {
		dialect string
		want    pb.AILogGranularity
	}{
		{"dsh-sse", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN},
		{"mock-stdio", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN},
		{"acp-stdio", pb.AILogGranularity_AI_LOG_GRANULARITY_TURN},
		{"maka-runtime-host", pb.AILogGranularity_AI_LOG_GRANULARITY_TURN},
		{"", pb.AILogGranularity_AI_LOG_GRANULARITY_UNSPECIFIED},
		{"future-dialect", pb.AILogGranularity_AI_LOG_GRANULARITY_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := granularityOf(c.dialect); got != c.want {
			t.Errorf("granularityOf(%q)=%v, want %v", c.dialect, got, c.want)
		}
	}
}

// TestGetAIInteractionLog_GranularityAndChannelMeta — 条目在场：主面与各通道
// granularity 同值下发（同条目同方言）。
func TestGetAIInteractionLog_GranularityAndChannelMeta(t *testing.T) {
	s := &RuntimeServiceImpl{aiLogs: newAILogStore("")} // 纯内存档（条目在场粒度面）
	e := s.aiLogs.writer("gw-b4-live", pb.AILogGranularity_AI_LOG_GRANULARITY_TURN)
	e.write([]byte("主面内容"))
	e.writeChannel("n001", []byte("通道内容"))
	e.finish()

	resp, err := s.GetAIInteractionLog(context.Background(), &pb.GetAIInteractionLogRequest{TaskId: "gw-b4-live"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetGranularity() != pb.AILogGranularity_AI_LOG_GRANULARITY_TURN {
		t.Fatalf("主面粒度未下发: %v", resp.GetGranularity())
	}
	if len(resp.GetChannels()) != 1 {
		t.Fatalf("通道数: %d", len(resp.GetChannels()))
	}
	if g := resp.GetChannels()[0].GetGranularity(); g != pb.AILogGranularity_AI_LOG_GRANULARITY_TURN {
		t.Fatalf("通道粒度未下发: %v", g)
	}
}

// TestGetAIInteractionLog_EvictedChannelDiskFallback — TODO#13：条目淘汰后
// （内存注册表无此任务）Get 的通道不回空——channelDeltas 盘回落（.n-*.ai.log）；
// 粒度不可知=UNSPECIFIED（前端回落 agent 判别）。
func TestGetAIInteractionLog_EvictedChannelDiskFallback(t *testing.T) {
	s := &RuntimeServiceImpl{aiLogs: newAILogStore(t.TempDir())} // 落盘档（盘回落面）
	e := s.aiLogs.writer("gw-b4-evict", pb.AILogGranularity_AI_LOG_GRANULARITY_TOKEN)
	e.write([]byte("主面定格"))
	e.writeChannel("n001", []byte("节点一通道内容"))
	e.writeChannel("n002", []byte("节点二通道内容"))
	e.finish()
	// 模拟 LRU 淘汰：注册表移除（盘档仍在）
	s.aiLogs.mu.Lock()
	delete(s.aiLogs.logs, "gw-b4-evict")
	s.aiLogs.mu.Unlock()

	// MaxBytes 缺省（0）=网关标准路径形态——归一后单响应封顶（对抗式审查 P2-2：
	// 修复前 0=无上限，盘回落首帧可整发 16MB×(1+节点数)）
	resp, err := s.GetAIInteractionLog(context.Background(), &pb.GetAIInteractionLogRequest{TaskId: "gw-b4-evict"})
	if err != nil {
		t.Fatal(err)
	}
	if limit := int(aiLogReadDefault); len(resp.GetChunk()) > limit {
		t.Fatalf("盘回落主面未封顶: %d > %d", len(resp.GetChunk()), limit)
	}
	for _, ch := range resp.GetChannels() {
		if limit := int(aiLogReadDefault); len(ch.GetChunk()) > limit {
			t.Fatalf("盘回落通道 %s 未封顶: %d > %d", ch.GetNodeId(), len(ch.GetChunk()), limit)
		}
	}
	if len(resp.GetChannels()) != 2 {
		t.Fatalf("条目淘汰后通道回空（TODO#13 回归）: %d", len(resp.GetChannels()))
	}
	for _, ch := range resp.GetChannels() {
		if ch.GetTotalBytes() == 0 {
			t.Fatalf("通道 %s 盘回落内容空", ch.GetNodeId())
		}
		if ch.GetGranularity() != pb.AILogGranularity_AI_LOG_GRANULARITY_UNSPECIFIED {
			t.Fatalf("盘回落粒度应 UNSPECIFIED（诚实）: %v", ch.GetGranularity())
		}
	}
}
