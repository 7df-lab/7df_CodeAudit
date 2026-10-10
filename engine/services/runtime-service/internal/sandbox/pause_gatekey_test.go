// 派生回合闸门归一测试：
// 补丁回合（TaskID+"-fix"）经 gateKey 共享主任务闸门与冻结锚——修复前独立寻址
// =PAUSED 态照常开新沙箱跑完（gate miss）+冻结回调 miss（锚丢失）。
package sandbox

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateKey_UnifiesDerivedRoundSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gw-abc", "gw-abc"},             // 根任务：恒等
		{"gw-abc-fix", "gw-abc"},         // 补丁回合：归一
		{"gw-abc-fixup", "gw-abc-fixup"}, // 非派生后缀：不误剥
		{"-fix", "-fix"},                 // 根名剥后成空：不归一（防空寻址键）
		{"", ""},
	}
	for _, c := range cases {
		if got := gateKey(c.in); got != c.want {
			t.Errorf("gateKey(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestWaitFrozen_DerivedRoundSharesGateAndAnchor：PauseSession(根) 后，派生回合
// 寻址（gateKey("…-fix")）应命中同一闸门并执行根任务冻结锚；ResumeSession(根)
// 释放。修复前：GateFor("-fix" 键) 拿到独立未暂停 gate 直通、锚永不执行。
func TestWaitFrozen_DerivedRoundSharesGateAndAnchor(t *testing.T) {
	const root = "T-gk-1"
	derived := root + "-fix"
	ClearSession(root)
	ClearFrozen(root)
	defer ClearSession(root)
	defer ClearFrozen(root)

	var anchored int32
	OnFrozen(root, func() { atomic.AddInt32(&anchored, 1) }) // 主回合 registerBatonFreeze 的等价注册

	PauseSession(root) // PauseTask 挂根任务
	g := GateFor(gateKey(derived))

	errc := make(chan error, 1)
	go func() { errc <- g.WaitFrozen(context.Background(), gateKey(derived)) }()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&anchored) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := atomic.LoadInt32(&anchored); n != 1 {
		t.Fatalf("派生回合挂起应执行根任务冻结锚（锚丢失=S2 发现②）: anchored=%d", n)
	}
	select {
	case err := <-errc:
		t.Fatalf("挂起期间派生回合不应放行（S2 发现①=PAUSED 态照常跑）: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	ResumeSession(root)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("resume 后放行: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume 后派生回合未放行")
	}
	if n := atomic.LoadInt32(&anchored); n != 1 {
		t.Fatalf("锚应恰执行一次: %d", n)
	}
}

// TestRunSession_GateWiringUsesGateKey — 静态接线守卫（R115 funcSeg 同手法）：
// 会话闸门消费点必须经 gateKey 寻址。行为测试直调 gateKey 无法感知 RunSession
// 接线面回归（消融实证：session.go 回退直寻址时行为测试仍绿）——生产消费点的
// 接线文本是唯一可靠锚。
func TestRunSession_GateWiringUsesGateKey(t *testing.T) {
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatal(err)
	}
	seg := funcSeg(t, string(src), "func (r *ManagerRunner) RunSession")
	if !strings.Contains(seg, "GateFor(gateKey(t.TaskID))") {
		t.Fatal("S2①②: RunSession 会话闸门寻址未经 gateKey（派生回合独立寻址=闸门/锚双 miss）")
	}
	if !strings.Contains(seg, "gate.WaitFrozen(ctx, gateKey(t.TaskID))") {
		t.Fatal("S2①②: RunSession 回合闸门等待未经 gateKey（冻结锚寻址回退）")
	}
}

// TestRun_GateWiringUsesGateKey — Run 面静态接线守卫（对抗式审查 P2-1 修复锚）：
// Run 是主回合+补丁回合（-fix）共用的回合发起单点，闸门必须在此消费——只锚
// RunSession 会让补丁回合静默回到"PAUSED 照常跑"（S2 发现①的真正修复点）。
func TestRun_GateWiringUsesGateKey(t *testing.T) {
	src, err := os.ReadFile("sandbox.go")
	if err != nil {
		t.Fatal(err)
	}
	seg := funcSeg(t, string(src), "func (r *ManagerRunner) Run(")
	if !strings.Contains(seg, "GateFor(gateKey(t.TaskID))") {
		t.Fatal("R129①: Run 回合发起面未接闸门（补丁回合 PAUSED 态照常开新沙箱——S2 发现①回归）")
	}
	if !strings.Contains(seg, "WaitFrozen(ctx, gateKey(t.TaskID))") {
		t.Fatal("R129①②: Run 闸门等待未经 gateKey（冻结锚寻址回退）")
	}
}
