package service

// TP20-T2 切换 Agent 收口（计划 §2.4 用例①④）：ListAgents 清单契约（含文件缺失
// 消融）+ 冻结回调 AI marker（含无 agent_to 零 marker 消融）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// TestListAgents_RepoRegistry — 清单契约：注册表如实投影+首条缺省+五布尔映射。
// providersPath() 经 go-config walkUp 落 engine/configs/agent_providers.yaml（仓内
// 真实注册表，与 descriptor_test TestMakaEntryInRepoConfig 同源锚定）；映射正确性
// 以 sandbox.ListDescriptors 同路径输出为期望（锁 handler 映射而非复制文件内容）。
func TestListAgents_RepoRegistry(t *testing.T) {
	s := &RuntimeServiceImpl{}
	resp, err := s.ListAgents(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	entries, err := sandbox.ListDescriptors(providersPath())
	if err != nil {
		t.Fatalf("ListDescriptors: %v", err)
	}
	if len(resp.GetAgents()) != len(entries) || len(entries) == 0 {
		t.Fatalf("条目数不符: resp=%d entries=%d", len(resp.GetAgents()), len(entries))
	}
	defaults := 0
	for i, a := range resp.GetAgents() {
		e := entries[i]
		if a.GetId() != e.ID || a.GetDialect() != e.Dialect {
			t.Errorf("#%d id/dialect 漂移: %s/%s want %s/%s", i, a.GetId(), a.GetDialect(), e.ID, e.Dialect)
		}
		if a.GetIsDefault() != (i == 0) {
			t.Errorf("#%d is_default 应=首条判定(%v): %s", i, i == 0, a.GetId())
		}
		if a.GetIsDefault() {
			defaults++
		}
		// 五布尔=capabilities 如实声明（未声明=false 保守缺省）
		if a.GetToolEvents() != e.Capabilities.ToolEvents ||
			a.GetStructuredSubmit() != e.Capabilities.StructuredSubmit ||
			a.GetInteraction() != e.Capabilities.Interaction ||
			a.GetSessionResume() != e.Capabilities.SessionResume ||
			a.GetConcludeSemantics() != e.Capabilities.ConcludeSemantics {
			t.Errorf("#%d capabilities 五布尔映射漂移: %+v want %+v", i, a, e.Capabilities)
		}
	}
	if defaults != 1 {
		t.Fatalf("is_default 恰一条, got %d", defaults)
	}
	// 仓内注册表形状锚（dsh=首条缺省+dsh-sse 方言——DefaultDescriptor 同型）
	if resp.GetAgents()[0].GetId() != "dsh" || resp.GetAgents()[0].GetDialect() != "dsh-sse" {
		t.Errorf("首条应为 dsh/dsh-sse: %s/%s", resp.GetAgents()[0].GetId(), resp.GetAgents()[0].GetDialect())
	}
}

// TestSetSwitchAgent_Registry — 暂存语义：一次消费即清（挂起至下一次冻结不重复）。
func TestSetSwitchAgent_Registry(t *testing.T) {
	setSwitchAgent("T-sw", "maka")
	if got := takeSwitchAgent("T-sw"); got != "maka" {
		t.Fatalf("take: %q", got)
	}
	if got := takeSwitchAgent("T-sw"); got != "" {
		t.Fatalf("二次 take 应清空: %q", got)
	}
}

// freezeWithMarker — 触发一次冻结（回调同步执行于 WaitFrozen 内）。
func freezeWithMarker(t *testing.T, taskID string) {
	t.Helper()
	sandbox.PauseSession(taskID)
	defer sandbox.ResumeSession(taskID)
	go func() {
		time.Sleep(80 * time.Millisecond)
		sandbox.ResumeSession(taskID)
	}()
	if err := sandbox.GateFor(taskID).WaitFrozen(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
}

// TestFreezeMarker_SwitchLine — 用例④主列：agent_to 暂存非空 → 冻结回调向人性化流
// 写「── 切换 Agent from → to ──」。from=回调 agent 参数；空（4a/4b/图路径同型
// 注册）回落注册表首条缺省 id（部署缺省语义，与投影链 agent 解析同源）。
func TestFreezeMarker_SwitchLine(t *testing.T) {
	dir := t.TempDir()
	human := filepath.Join(dir, "t-marker.ai.log")
	entry := &aiLogEntry{humanPath: human}
	release := registerBatonFreeze(batonPen{TaskID: "T-marker", Entry: entry, Origin: nil, Agent: ""}, "对项目做安全审计", dir)
	defer release()
	setSwitchAgent("T-marker", "maka")
	freezeWithMarker(t, "T-marker")
	raw, err := os.ReadFile(human)
	if err != nil {
		t.Fatalf("人性化流未落盘: %v", err)
	}
	// from：agent 参数空 → 注册表首条（仓内=dsh）
	if !strings.Contains(string(raw), "── 切换 Agent dsh → maka ──") {
		t.Fatalf("marker 文本缺失/不符:\n%s", raw)
	}
	if !strings.Contains(entry.buf.String(), "── 切换 Agent dsh → maka ──") {
		t.Fatal("marker 未进内存流（RPC 数据源）")
	}
}

// TestFreezeMarker_NoTargetAblation — 用例④消融列：无 agent_to（纯暂停）零 marker。
func TestFreezeMarker_NoTargetAblation(t *testing.T) {
	dir := t.TempDir()
	entry := &aiLogEntry{humanPath: filepath.Join(dir, "t-plain.ai.log")}
	release := registerBatonFreeze(batonPen{TaskID: "T-plain", Entry: entry, Origin: nil, Agent: ""}, "对项目做安全审计", dir)
	defer release()
	freezeWithMarker(t, "T-plain")
	if strings.Contains(entry.buf.String(), "切换 Agent") {
		t.Fatalf("纯暂停不得写 marker:\n%s", entry.buf.String())
	}
}

// TestFreezeMarker_FromAgentParam — from=非空 agent 参数直用（audit 路径请求级 agent）。
func TestFreezeMarker_FromAgentParam(t *testing.T) {
	dir := t.TempDir()
	entry := &aiLogEntry{humanPath: filepath.Join(dir, "t-from.ai.log")}
	release := registerBatonFreeze(batonPen{TaskID: "T-from", Entry: entry, Origin: nil, Agent: "maka-acp"}, "goal", dir)
	defer release()
	setSwitchAgent("T-from", "dsh")
	freezeWithMarker(t, "T-from")
	if !strings.Contains(entry.buf.String(), "── 切换 Agent maka-acp → dsh ──") {
		t.Fatalf("from 应=回调 agent 参数:\n%s", entry.buf.String())
	}
}
