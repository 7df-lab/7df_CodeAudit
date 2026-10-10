package pack

// 桌面演练与仓内 fixture 校验（切片 2 DoD 末项："schema 草案经一次写第二份 pack 桌面
// 演练不返工"）。依据: 主仓库 docs/designs/platform-slice2-pack-schema.md §5/§6。

import (
	"os"
	"strings"
	"testing"
)

const packsDir = "../../../../configs/packs" // engine/configs/packs（仓内事实源）

// 仓内第一份 pack（审计回填）必须随 schema 演进保持可解析——文件与校验器同步锁。
func TestRepoAuditSamplePackValidates(t *testing.T) {
	data, err := os.ReadFile(packsDir + "/audit-sample.yaml")
	if err != nil {
		t.Skipf("仓内 packs 目录不可达（非仓根检出）: %v", err)
	}
	p, err := Parse(data, nil)
	if err != nil {
		t.Fatalf("repo audit-sample.yaml must validate: %v", err)
	}
	if p.Name != "audit-sample" || p.Spec.Image == "" || p.Spec.Dialect != "dsh-sse" {
		t.Fatalf("repo pack content wrong: %+v", p)
	}
}

// 渗透 pack v0 转正（切片 5+6 §5，ADR-249）：pentest.yaml 以内建集通过（schema_ref
// 已定稿 findings/submit_findings@v1——L1 裁决，无需扩集）；草稿 .draft 已删。
func TestRepoPentestPackValidates(t *testing.T) {
	data, err := os.ReadFile(packsDir + "/pentest.yaml")
	if err != nil {
		t.Skipf("仓内 packs 目录不可达（非仓根检出）: %v", err)
	}
	p, err := Parse(data, nil)
	if err != nil {
		t.Fatalf("repo pentest.yaml must validate under default refs: %v", err)
	}
	if p.Spec.Execution != "graph" || p.Spec.Network.Profile != "targeted" {
		t.Fatalf("pentest execution/profile = %q/%q, want graph/targeted", p.Spec.Execution, p.Spec.Network.Profile)
	}
	if len(p.Spec.Graph.SeedIntents) == 0 {
		t.Fatalf("pentest seed_intents 不可为空（初始攻击面清单，§5）")
	}
	// 靶标不入 pack：pack 里不得出现靶标字段
	if strings.Contains(string(data), "target_hosts") || strings.Contains(string(data), "authorized_targets") {
		t.Fatal("pack must not carry target fields（靶标=task 资源字段）")
	}
}
