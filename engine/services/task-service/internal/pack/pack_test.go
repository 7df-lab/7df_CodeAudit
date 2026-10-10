package pack

// pack v0 校验与 revision 语义测试（切片 2 DoD）：
//   - 加载器：合法通过；未知字段/缺失引用/浮动引用三类拒载（各给字段路径）；
//   - revision 表：哈希命中复用 / 回退产生新修订 / 任务钉死修订版 三语义；
// 依据: 主仓库 docs/designs/platform-slice2-pack-schema.md §2/§3/§6。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validYAML() []byte {
	return []byte(`apiVersion: codeaudit.pack/v0
kind: WorkloadPack
name: audit-sample
description: 代码审计示例负载
spec:
  image: dsh-pentest-sse:latest
  dialect: dsh-sse
  network:
    profile: restricted
  seeds:
    brief_renderer: baton-brief
    render_target: task-card-prefix
    template_ref: assignmentTemplate@frozen
  results:
    schema_ref: findings/submit_findings@v1
    parser: codeaudit-submit
  timeouts:
    per_turn: default
    task_total: audit-default
  constraints_schema: [severity_threshold, language_hint]
`)
}

// 合法 pack 通过 + 哈希稳定。
func TestParse_ValidPack(t *testing.T) {
	p, err := Parse(validYAML(), nil)
	if err != nil {
		t.Fatalf("valid pack rejected: %v", err)
	}
	if p.Name != "audit-sample" || p.Spec.Dialect != "dsh-sse" {
		t.Fatalf("parsed fields wrong: %+v", p)
	}
	h1, _ := p.SpecHash()
	h2, _ := p.SpecHash()
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("spec hash unstable: %s vs %s", h1, h2)
	}
}

// 三类拒载（各带字段路径）：未知字段 / 缺失引用 / 浮动引用。
func TestParse_RejectClasses(t *testing.T) {
	// 1) 未知字段（防部分应用，U6 同型）
	bad := strings.Replace(string(validYAML()), "spec:\n", "spec:\n  unknown_knob: 1\n", 1)
	if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), "unknown_knob") {
		t.Fatalf("unknown field must be rejected naming it, got %v", err)
	}
	// 2) 缺失引用（dialect 不在注册表）
	bad = strings.Replace(string(validYAML()), "dialect: dsh-sse", "dialect: nope-dialect", 1)
	if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), "spec.dialect") {
		t.Fatalf("missing dialect ref must fail with field path, got %v", err)
	}
	// 3) 浮动引用（@latest 拒载；C3 禁浮动）
	bad = strings.Replace(string(validYAML()), "assignmentTemplate@frozen", "assignmentTemplate@latest", 1)
	if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), "spec.seeds.template_ref") {
		t.Fatalf("floating ref must fail with field path, got %v", err)
	}
	bad = strings.Replace(string(validYAML()), "findings/submit_findings@v1", "findings/submit_findings@head", 1)
	if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), "spec.results.schema_ref") {
		t.Fatalf("floating schema ref must fail with field path, got %v", err)
	}
	// 4) schema_ref id 不在注册表
	bad = strings.Replace(string(validYAML()), "findings/submit_findings@v1", "pentest/report@v1", 1)
	if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), "spec.results.schema_ref") {
		t.Fatalf("unknown schema ref id must fail, got %v", err)
	}
}

// 头契约/name/描述等基础面拒载。
func TestParse_HeaderContract(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{"codeaudit.pack/v0", "codeaudit.pack/v1", "apiVersion"},
		{"WorkloadPack", "CronJob", "kind"},
		{"name: audit-sample", "name: Audit_Sample", "name"},
		{"description: 代码审计示例负载", "description: \"\"", "description"},
	}
	for _, c := range cases {
		bad := strings.Replace(string(validYAML()), c.old, c.new, 1)
		if _, err := Parse([]byte(bad), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s→%s: want %q error, got %v", c.old, c.new, c.want, err)
		}
	}
}

// revision 三语义（DoD）。
func TestRegistry_ThreeSemantics(t *testing.T) {
	reg := NewRegistry()
	now := time.Now()
	reg.now = func() time.Time { now = now.Add(time.Second); return now }

	p1, err := Parse(validYAML(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// ① 哈希命中复用：同 spec 重复提交不产生新 revision
	rev1, reused, err := reg.Register(p1, "operator-a")
	if err != nil || reused {
		t.Fatalf("first register: %v reused=%v", err, reused)
	}
	rev1b, reused, err := reg.Register(p1, "operator-b")
	if err != nil || !reused || rev1b.Revision != rev1.Revision {
		t.Fatalf("same spec must reuse revision: reused=%v rev=%v vs %v", reused, rev1b.Revision, rev1.Revision)
	}
	// ② 修订推进 + 回退产生新修订（append-only，不改历史）
	changed := strings.Replace(string(validYAML()), "profile: restricted", "profile: targeted", 1)
	p2, _ := Parse([]byte(changed), nil)
	rev2, reused, err := reg.Register(p2, "operator-a")
	if err != nil || reused || rev2.Revision != 2 {
		t.Fatalf("spec change must append rev2, got reused=%v rev=%v", reused, rev2.Revision)
	}
	back, _ := Parse(validYAML(), nil) // 回退到历史 spec
	rev3, reused, err := reg.Register(back, "operator-a")
	if err != nil || reused || rev3.Revision != 3 {
		t.Fatalf("revert must append rev3 (append-only), got reused=%v rev=%v", reused, rev3.Revision)
	}
	if len(reg.History("audit-sample")) != 3 {
		t.Fatalf("history must keep 3 revisions, got %d", len(reg.History("audit-sample")))
	}
	// ③ 任务钉死修订版：创建时刻解析 latest，之后注册新修订不影响已钉引用
	pinned, err := reg.PinLatest("audit-sample")
	if err != nil || pinned.ID() != "audit-sample@3" {
		t.Fatalf("pin latest = %v (err %v)", pinned, err)
	}
	newer := strings.Replace(changed, "parser: codeaudit-submit", "parser: text-final", 1)
	p4, _ := Parse([]byte(newer), nil)
	if _, _, err := reg.Register(p4, "operator-a"); err != nil {
		t.Fatal(err)
	}
	// 已钉引用（rev1 的 ID/哈希）不被后续注册改写：
	if pinned.ID() != "audit-sample@3" || pinned.SpecHash != rev3.SpecHash {
		t.Fatalf("pinned revision must stay frozen: %+v", pinned)
	}
	// 未知 pack：fail-fast（不静默回落缺省负载）
	if _, err := reg.PinLatest("no-such-pack"); err == nil {
		t.Fatal("unknown pack must fail loud")
	}
}

// LoadDir：仅 *.yaml 入加载面；.draft 草稿跳过；同名重复即错。
func TestLoadDir_FiltersAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), validYAML(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "draft.yaml.draft"), validYAML(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("apiVersion: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packs, err := LoadDir(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("invalid yaml must fail naming file, got %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "bad.yaml")); err != nil {
		t.Fatal(err)
	}
	packs, err = LoadDir(dir, nil)
	if err != nil || len(packs) != 1 {
		t.Fatalf("draft must be skipped, valid loaded=%d err=%v", len(packs), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), validYAML(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir, nil); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("duplicate pack name must fail, got %v", err)
	}
}
