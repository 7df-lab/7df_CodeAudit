package sandbox

// ADR-248 L2 结构守卫：分型接线不变量 + 跨仓模板一致性条件门禁。
// 形态沿 pg_bounded/workspace_override 结构守卫先例（源码字符串断言——锁接线
// 面，行为面由 policy_compose_test 承担）。

import (
	"os"
	"strings"
	"testing"
)

// TestNetworkPolicy_Wired — launch 消费唯一构造点（sandboxSpecWithPolicy），
// 不存在绕过点直拼 spec；组合器是唯一 endpoints 写入点（B1 单向收紧）。
func TestNetworkPolicy_Wired(t *testing.T) {
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "sandboxSpecWithPolicy(name, taskID, r.cfg)") {
		t.Fatal("ADR-248: launch 必须经 sandboxSpecWithPolicy 唯一构造点携带分型")
	}
	if strings.Contains(string(src), `"spec": sandboxSpec(`) {
		t.Fatal("ADR-248: launch 存在绕过构造点的裸 sandboxSpec 直拼（分型断链）")
	}
	csrc, err := os.ReadFile("policy_compose.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(csrc), `tmpl.endpoint["endpoints"] = eps`); n != 1 {
		t.Fatalf("组合器须是唯一 endpoints 写入点（B1 单向收紧），写入语句出现 %d 次", n)
	}
}

// TestTargetedTemplate_CrossRepoConsistency — 跨仓一致性条件门禁：主仓库开发
// 环境（兄弟仓 dsh-pentest-sse 检出存在）时逐字节比对 engine 副本与原件；
// engine 单仓 fresh clone 无兄弟仓=跳过（主仓库层恒跑此对）。漂移即红：两份
// 模板是同一权威的两个消费面（镜像侧组合器参照/engine 侧组合源）。
func TestTargetedTemplate_CrossRepoConsistency(t *testing.T) {
	upstream := "../../../../../dsh-pentest-sse/policy/targeted.yaml"
	if _, err := os.Stat(upstream); err != nil {
		t.Skip("兄弟仓 dsh-pentest-sse 不在本检出（单仓 clone）——一致性由主仓库环境承担")
	}
	a, err := os.ReadFile(repoTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("engine 副本与 dsh-pentest-sse 原件漂移——两消费面必须逐字节一致（C3 锚）")
	}
}
