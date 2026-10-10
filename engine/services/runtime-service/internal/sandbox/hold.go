// handoff hold — 沙箱保活注册表（R4，切片 1；S2 优化项非正确性前提）。
// 依据: 主仓库 baton/docs/engine-integration-req.md R4（换手窗口内 teardown/对账跳过，
// 仅为免重解包的性能优化；正确性由 R3 快照保证——沙箱是易失资源，架构不许依赖其实例
// 连续性）；ADR-244（TTL 缺省与 env 覆盖口）。
//
// 语义：HoldSandbox(name, ttl) 在换手冻结点登记保活；teardown（RunSession 回合闸门
// 取消路径）与 SandboxReconciler 孤儿对账据此跳过回收；TTL 到期后 holds 失效，沙箱
// 由对账器按既有孤儿纪律回收（"窗口后正常回收"）。显式 ReleaseHold 供恢复路径提前
// 释放。注册表跨 runner 实例共享（同 activeSandboxes 包级形态）。
package sandbox

import (
	"os"
	"sync"
	"time"
)

// handoffHolds — 沙箱名 → 保活到期时刻。惰性过期：读时判定，不跑后台清扫器。
var handoffHolds sync.Map

// holdTTLDefault — 保活缺省窗口（ADR-244）：与 runtime.session.ttl_s=1800（07 §8
// "DSH 会话存活 30m"）同量级——换手窗口覆盖一个完整会话周期；env 可覆盖。
const holdTTLDefault = 30 * time.Minute

// HoldTTL — 保活窗口时长（env CODEAUDIT_SANDBOX_HOLD_TTL=Go duration；非法值=缺省）。
func HoldTTL() time.Duration {
	if v := os.Getenv("CODEAUDIT_SANDBOX_HOLD_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return holdTTLDefault
}

// HoldSandbox — 登记保活（同沙箱重复登记=续期）。
func HoldSandbox(name string, ttl time.Duration) {
	if name == "" {
		return
	}
	handoffHolds.Store(name, time.Now().Add(ttl))
}

// ReleaseHold — 显式释放（恢复路径/测试用）。
func ReleaseHold(name string) { handoffHolds.Delete(name) }

// sandboxHeld — 保活判定（惰性过期：到期即视同未保活并顺手清除）。
func sandboxHeld(name string) bool {
	v, ok := handoffHolds.Load(name)
	if !ok {
		return false
	}
	exp, _ := v.(time.Time)
	if time.Now().Before(exp) {
		return true
	}
	handoffHolds.Delete(name)
	return false
}
