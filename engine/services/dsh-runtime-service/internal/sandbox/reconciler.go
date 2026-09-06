package sandbox

// SandboxReconciler — 孤儿沙箱对账回收（ADR-210）。
//
// 背景：dsh-runtime-service 进程级死亡（重启/OOM/SSE panic 带崩）时，进程内 defer 的
// teardown 不再执行，manager 侧沙箱永久泄漏（status.md 有多次 6 连漏实证）。teardown
// 加固（如实+重试）只覆盖进程活着的情况，本组件兜底进程死过之后：
//
//	周期（启动 2min 后首轮，之后每 interval）经 manager GET /api/v1/sandboxes 列出全部
//	沙箱 → 过滤"本服务命名（^(am|ca)-[0-9a-f]{12}$，ca- 为现行、am- 为存量）且不在进程
//	活跃注册表"者 → DELETE。单副本部署语义下，服务重启后注册表为空、在途任务已随之
//	消亡，剩余本服务名沙箱即孤儿。
//
// 开关：env CODEAUDIT_SANDBOX_RECONCILE=off 关闭（main.go 接线处判定）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"
)

// activeSandboxes — 进程级活跃沙箱注册表（包级：ManagerRunner 每请求新建实例，
// 注册表必须跨实例共享）。launch 创建动作前 Store，teardown 结束时 Delete。
var activeSandboxes sync.Map

// sandboxActive — 供对账过滤与测试查询。
func sandboxActive(name string) bool {
	_, ok := activeSandboxes.Load(name)
	return ok
}

// sandboxNameRe — 本服务沙箱命名（ca- 现行 + am- 存量，ADR-210 前缀更替）。
var sandboxNameRe = regexp.MustCompile(`^(am|ca)-[0-9a-f]{12}$`)

// managerSandboxRef — manager 列表返回投影（gateway.py _ref）的最小消费面。
type managerSandboxRef struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// managedByLabel — 创建时打的归属标签（sandboxSpec），名字正则之外的第二重圈定。
const managedByLabel = "codeaudit-dsh-runtime"

// orphanNames — 纯函数：从 manager 列表投影中选出本服务孤儿（可测，无 IO）。
// 判据：名字匹配本服务命名规约（或带归属标签）且不在活跃注册表。
func orphanNames(refs []managerSandboxRef, isActive func(string) bool) []string {
	var out []string
	for _, ref := range refs {
		if ref.Name == "" || isActive(ref.Name) {
			continue
		}
		if sandboxNameRe.MatchString(ref.Name) || ref.Labels[managedByLabel] == "codeaudit-dsh-runtime" {
			out = append(out, ref.Name)
		}
	}
	return out
}

// SandboxReconciler — 周期对账器。
type SandboxReconciler struct {
	r            *ManagerRunner
	interval     time.Duration
	startupDelay time.Duration
	stopCh       chan struct{}
	mu           sync.Mutex
	running      bool
}

// ReconcilerOption — 函数式选项。
type ReconcilerOption func(*SandboxReconciler)

// WithReconcileInterval — 周期（默认 30m）。
func WithReconcileInterval(d time.Duration) ReconcilerOption {
	return func(c *SandboxReconciler) { c.interval = d }
}

// WithReconcileStartupDelay — 首轮延迟（默认 2m，避开启动抖动与 manager 未就绪）。
func WithReconcileStartupDelay(d time.Duration) ReconcilerOption {
	return func(c *SandboxReconciler) { c.startupDelay = d }
}

// NewSandboxReconciler — 构造（r 提供 manager 端点/凭据/call 通道）。
func NewSandboxReconciler(r *ManagerRunner, opts ...ReconcilerOption) *SandboxReconciler {
	c := &SandboxReconciler{
		r:            r,
		interval:     30 * time.Minute,
		startupDelay: 2 * time.Minute,
		stopCh:       make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Start — 启动对账循环（幂等）。
func (c *SandboxReconciler) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	go c.run()
	log.Printf("[sandbox-reconciler] started: startup_delay=%v interval=%v", c.startupDelay, c.interval)
}

// Stop — 停止。
func (c *SandboxReconciler) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	close(c.stopCh)
	c.running = false
	log.Println("[sandbox-reconciler] stopped")
}

func (c *SandboxReconciler) run() {
	select {
	case <-time.After(c.startupDelay):
	case <-c.stopCh:
		return
	}
	c.ReconcileOnce()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.ReconcileOnce()
		case <-c.stopCh:
			return
		}
	}
}

// ReconcileOnce — 单轮：列沙箱 → 选孤儿 → 逐个 DELETE（返回回收数；供单测/运维手动触发）。
func (c *SandboxReconciler) ReconcileOnce() int {
	url, token := c.r.managerEndpoint()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	body, err := c.r.call(ctx, "GET", url+"/api/v1/sandboxes?limit=500", token, nil)
	if err != nil {
		log.Printf("[sandbox-reconciler] list sandboxes failed: %v", err)
		return 0
	}
	refs, err := decodeSandboxRefs(body)
	if err != nil {
		log.Printf("[sandbox-reconciler] decode sandbox list failed: %v", err)
		return 0
	}
	orphans := orphanNames(refs, sandboxActive)
	n := 0
	for _, name := range orphans {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, derr := c.r.call(dctx, "DELETE", url+"/api/v1/sandboxes/"+name+"?workspace="+c.r.cfg.Workspace, token, nil); derr != nil {
			log.Printf("[sandbox-reconciler] delete orphan %s failed: %v", name, derr)
		} else {
			log.Printf("[sandbox-reconciler] orphan sandbox deleted: %s", name)
			n++
		}
		dcancel()
	}
	if len(orphans) > 0 {
		log.Printf("[sandbox-reconciler] pass done: orphans=%d deleted=%d", len(orphans), n)
	}
	return n
}

// decodeSandboxRefs — manager 列表固定返回 {"sandboxes":[...]}（api.py sandbox_list）。
func decodeSandboxRefs(body map[string]any) ([]managerSandboxRef, error) {
	if body == nil {
		return nil, nil
	}
	raw, ok := body["sandboxes"].([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected list shape: sandboxes field missing")
	}
	refs := make([]managerSandboxRef, 0, len(raw))
	for _, item := range raw {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var ref managerSandboxRef
		if err := json.Unmarshal(b, &ref); err == nil {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}
