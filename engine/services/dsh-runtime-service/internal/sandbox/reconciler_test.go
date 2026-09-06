package sandbox

// ADR-210 孤儿沙箱过滤纯函数测试——判据：本服务命名（^(am|ca)-hex12，ca- 现行/am- 存量）
// 或归属标签，且不在进程活跃注册表。

import (
	"testing"
)

func TestOrphanNames(t *testing.T) {
	active := map[string]bool{"ca-active000001": true} // 假名:长度不符正则,单测只验证 isActive 通道
	refs := []managerSandboxRef{
		{Name: "ca-0123456789ab"},                         // 孤儿(新前缀) → 删
		{Name: "am-0123456789ab"},                         // 孤儿(存量前缀) → 删
		{Name: "ca-active000001"},                         // 活跃 → 留
		{Name: "openshell-default--am-8306b7aef2d6-xyz"},  // 他人沙箱(命名不符) → 留
		{Name: "zz-unrelated"},                            // 无关 → 留
		{Name: "ca-short"},                                // 前缀对但长度不符 → 留(防误伤)
		{Name: "future-prefix-xyz", Labels: map[string]string{managedByLabel: "codeaudit-dsh-runtime"}}, // 名字不合正则但带归属标签 → 删(标签兜底)
		{Name: "future-prefix-abc", Labels: map[string]string{"managed-by": "someone-else"}}, // 他人标签 → 留
	}
	got := orphanNames(refs, func(n string) bool { return active[n] })
	want := []string{"ca-0123456789ab", "am-0123456789ab", "future-prefix-xyz"}
	if len(got) != len(want) {
		t.Fatalf("orphanNames = %v, want %d items", got, len(want))
	}
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Fatalf("orphanNames missing %q (got %v)", w, got)
		}
	}
}

func TestSandboxNameRe(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"ca-0123456789ab", true},
		{"am-0123456789ab", true},
		{"ca-0123456789ABC", false}, // 大写 hex 不符
		{"ca-0123456789a", false},   // 11 位
		{"ca-0123456789abc", false}, // 13 位
		{"cb-0123456789ab", false},
	}
	for _, c := range cases {
		if got := sandboxNameRe.MatchString(c.name); got != c.want {
			t.Errorf("sandboxNameRe(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecodeSandboxRefs(t *testing.T) {
	body := map[string]any{
		"sandboxes": []any{
			map[string]any{"name": "ca-0123456789ab", "labels": map[string]any{managedByLabel: "codeaudit-dsh-runtime"}},
			map[string]any{"name": "other"},
		},
	}
	refs, err := decodeSandboxRefs(body)
	if err != nil || len(refs) != 2 {
		t.Fatalf("decodeSandboxRefs: refs=%v err=%v", refs, err)
	}
	if refs[0].Name != "ca-0123456789ab" || refs[0].Labels[managedByLabel] != "codeaudit-dsh-runtime" {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if _, err := decodeSandboxRefs(map[string]any{"unexpected": 1}); err == nil {
		t.Fatal("形态不符应报错而非静默通过")
	}
}
