// baton 投影触发钩子（TP17-T1）：分析回合收束后把 .sse.log 经 baton CLI 投影为
// 任务工作区 .tasklog/（中立事件档案，换手种子）。
// 依据: 主仓库 baton/docs/engine-integration-req.md R1（触发钩子）/R7（goal 注入）
// 依据: ADR-237（子进程解耦/BATON_BIN/goal 摘要/输出目录/降级纪律）
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

const (
	batonGoalMaxRunes = 120              // 依据: ADR-237（spine.goal 一句话语义的截断上限）
	batonExecTimeout  = 90 * time.Second // 依据: ADR-237（独立 context：defer 时调用方 ctx 可能已 cancel）
)

// batonBin — baton CLI 二进制路径（ADR-237 子进程解耦：baton 不进本仓依赖树，
// 部署层卷挂载；空/不存在=钩子禁用，诚实降级 log）。
func batonBin() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return ""
	}
	v, err := cfg.Str("runtime.sandbox.baton_bin", "BATON_BIN")
	if err != nil {
		return ""
	}
	return v
}

// tasklogDir — .tasklog/ 档案根；每任务 <dir>/<taskID>/ 即逻辑工作区根。
// 缺省派生 <interaction_dir>/../tasklog（ADR-237：与 .sse.log 同卷，便于换手种子分发）。
func tasklogDir() string {
	cfg, err := codeauditcfg.Default()
	if err == nil {
		if v, err := cfg.Str("runtime.sandbox.tasklog_dir", "CODEAUDIT_TASKLOG_DIR"); err == nil && v != "" {
			return v
		}
	}
	if dir := interactionDir(); dir != "" {
		return filepath.Join(filepath.Dir(dir), "tasklog")
	}
	return ""
}

// assignmentSummary — goal 摘要（R7 最小满足）：任务卡首行截断。
func assignmentSummary(assignment string) string {
	s := strings.TrimSpace(assignment)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > batonGoalMaxRunes {
		r = r[:batonGoalMaxRunes]
	}
	return strings.TrimSpace(string(r))
}

// batonProjectTurn — 回合收束投影（R1）：.sse.log → <tasklogDir>/<taskID>/.tasklog/。
// 降级纪律（ADR-237，同 .sse.log 落盘"留存通道不得影响分析主链路"）：任何失败只
// log 不上抛；baton project 幂等重跑，重复调用安全。
// StartupDegradationNotices — TP20 缺损批 B13：启动期降级面自检。
// brief/marker/baton 投影链依赖 BATON_BIN+tasklog 目录；此前缺配置=静默禁用档
// （诚实降级 ⚑2），但部署面无任何提示——上下文重建悄然退化为无前情段/无档案。
// 本函供 cmd/main 启动时打 WARN（覆盖 sim/prod 全部署面）。
func StartupDegradationNotices() []string {
	var out []string
	if batonBin() == "" {
		out = append(out, "BATON_BIN 未配置——接续简报/档案投影禁用档（上下文重建降级为无前情段，切换 Agent 续跑面受影响）")
	}
	if tasklogDir() == "" {
		out = append(out, "tasklog 目录未配置——档案投影/接续简报禁用档（同上）")
	}
	return out
}

// batonPen — TP20 缺损批 A10b：投影链核心参数团（taskID/entry/
// origin/agent 五函数结伴=Fowler Data Clumps）收敛为单类型；goal/assignment/
// projectPath 语义随调用点异，不入团。方言路径变体（maka/acp/dialect）形状
// 异构不强并。
type batonPen struct {
	TaskID string
	Entry  *aiLogEntry
	Origin *pb.TaskOrigin
	Agent  string
}

func batonProjectTurn(p batonPen, assignment string) {
	taskID, aiEntry, origin, agent := p.TaskID, p.Entry, p.Origin, p.Agent // A10b 别名：正文零改
	tlDir := tasklogDir()                                                  // 三处路径共用（Default() sync.Once 缓存，同值）
	// Maka 方言分叉：maka 回合无 .sse.log（无 bridge
	// 通道），RuntimeEventLog 已由 makaSession 在沙箱回收前导出至 maka-events.jsonl
	// ——存在即走 -maka 投影（spec 全量 YAML→中立事件九族映射，金丝雀 park 同档）。
	makaPath := filepath.Join(tlDir, taskID, "maka-events.jsonl")
	if st, err := os.Stat(makaPath); err == nil && st.Size() > 0 {
		batonProjectTurnMaka(taskID, assignment, makaPath, origin, agent)
		return
	}
	// ACP 方言分叉：acpSession 每回合导出的帧流（JSONL）
	// 存在即走 -acp 投影（转译器复用 maka 模式——baton internal/project/acp.go）。
	acpPath := filepath.Join(tlDir, taskID, "acp-events.jsonl")
	if st, err := os.Stat(acpPath); err == nil && st.Size() > 0 {
		batonProjectTurnAcp(taskID, assignment, acpPath, origin, agent)
		return
	}
	if aiEntry == nil || aiEntry.rawPath == "" {
		return // 落盘未配置：无输入不投影
	}
	if _, err := os.Stat(aiEntry.rawPath); err != nil {
		return // 本回合零帧（.sse.log 未产生）——无输入不投影
	}
	goal := assignmentSummary(assignment)
	if goal == "" {
		return
	}
	bin := batonBin()
	if bin == "" {
		return // 未配置=钩子禁用（部署编排接线后自动启用）
	}
	if _, err := os.Stat(bin); err != nil {
		log.Printf("[runtime][%s] baton hook disabled: BATON_BIN=%s not found", taskID, bin)
		return
	}
	dir := tlDir
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batonExecTimeout)
	defer cancel()
	// R7 完整版（TP17-T4）：tenant/user/constraints 自任务实体贯通（origin 可空=
	// 上游未填，档案不带这些维度）；-agent=R6 首批（cfg 驱动持笔人标识）。
	args := []string{"project",
		"-sse", aiEntry.rawPath, "-out", filepath.Join(dir, taskID),
		"-task", taskID, "-goal", goal, "-agent", agentOrEnv(agent)}
	// 切片 5+6（ADR-249 L1）：图任务的第二输入——graph.log 存在才传（双输入归并
	// ts graph-first 在 baton 侧；非图任务零变化）。
	if aiEntry.graphPath != "" {
		if _, err := os.Stat(aiEntry.graphPath); err == nil {
			args = append(args, "-graph", aiEntry.graphPath)
		}
	}
	if origin != nil {
		if origin.GetUser() != "" {
			args = append(args, "-user", origin.GetUser())
		}
		if origin.GetTenant() != "" {
			args = append(args, "-tenant", origin.GetTenant())
		}
		for _, c := range origin.GetConstraints() {
			args = append(args, "-constraint", c)
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[runtime][%s] baton project FAILED (degraded, archive only): %v\n%s",
			taskID, err, out)
		return
	}
	log.Printf("[runtime][%s] baton project ok: %s", taskID, strings.TrimSpace(string(out)))
}

// agentOrEnv — 持笔标识归一：任务级 agent（RunRequest
// →RPC req.agent 全链）优先；空=回落部署级 env（4a/4b 与图路径现状——图路径
// 任务级 agent 缺口 a4a271c 注记）。换手后接续回合的 pen 随新 agent（此前恒
// 部署缺省=换手档案 pen 失真，D2 首轮实证抓获）。
func agentOrEnv(agent string) string {
	if agent != "" {
		return agent
	}
	return agentID()
}

// agentID — 持笔 harness 标识（R6 首批/TP17-T4）：cfg 驱动（baton -agent 旗标，
// 默认 dsh）；descriptor 条目化后每个 agent 条目自带标识，此处即条目键。
func agentID() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return "dsh"
	}
	v, err := cfg.Str("runtime.sandbox.agent_id", "CODEAUDIT_AGENT_ID")
	if err != nil || v == "" {
		return "dsh"
	}
	return v
}

// gitAnchorOf — 工作区 git 基准（R3：可得则锚；非 git/无 commit → 空）。
func gitAnchorOf(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", projectPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// workspaceTreeRef — 冻结点工作区快照（R3 tree_ref v0，切片 1）：工作区树 tar.gz 内容
// 寻址落盘 <tasklogDir>/<taskID>/workspace/tree-<sha256>.tar.gz，返回引用 "sha256:<hex>"。
// 依据: 主仓库 baton/docs/engine-integration-req.md R3（handoff.checkpointed.workspace.
// tree_ref 可被离线解析回源码树）；主仓库 docs/designs/platform-slice1-r6-descriptor.md
// 切片 1 范围（tree_ref）；ADR-244（v0 落点=tasklog 换手种子卷——与 .tasklog 同卷分发，
// baton README "桶级 tree_ref"的 v0 形态；跨主机桶引用随 manager API 评估升档）。
// 降级纪律同投影：任何失败只 log 不上抛、不产生引用（宁缺不错）。
func workspaceTreeRef(taskID, projectPath string) string {
	if projectPath == "" {
		return ""
	}
	tlDir := tasklogDir()
	if tlDir == "" {
		return ""
	}
	gz, err := sandbox.TarProjectTree(projectPath)
	if err != nil {
		log.Printf("[runtime][%s] tree_ref 快照失败（降级，仅锚已有基线）: %v", taskID, err)
		return ""
	}
	sum := sha256.Sum256(gz)
	hexSum := hex.EncodeToString(sum[:])
	dir := filepath.Join(tlDir, taskID, "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[runtime][%s] tree_ref 目录创建失败（降级）: %v", taskID, err)
		return ""
	}
	archive := filepath.Join(dir, "tree-"+hexSum+".tar.gz")
	if err := os.WriteFile(archive, gz, 0o644); err != nil {
		log.Printf("[runtime][%s] tree_ref 快照落盘失败（降级）: %v", taskID, err)
		return ""
	}
	return "sha256:" + hexSum
}

// batonHandoffCheckpoint — 冻结基线锚（R2/R3，TP17-T2）：baton handoff checkpointed
// append（log_seq 由 baton 按档案现状定格；幂等防重在 baton 侧）。降级纪律同投影。
// 切片 1：-tree-ref 接线（R3）——工作区快照引用可得则传，baton 侧字段/旗标已预留。
func batonHandoffCheckpoint(taskID, projectPath string) {
	bin := batonBin()
	if bin == "" {
		return
	}
	hdDir := tasklogDir()
	if hdDir == "" {
		return
	}
	wsRoot := filepath.Join(hdDir, taskID)
	if _, err := os.Stat(filepath.Join(wsRoot, ".tasklog", "events.jsonl")); err != nil {
		return // 无档案（预约冻结/零回合）：无基线可锚——T1 投影未发生时不空锚
	}
	args := []string{"handoff", "-in", wsRoot}
	if anchor := gitAnchorOf(projectPath); anchor != "" {
		args = append(args, "-git-anchor", anchor)
	}
	if ref := workspaceTreeRef(taskID, projectPath); ref != "" {
		args = append(args, "-tree-ref", ref)
	}
	ctx, cancel := context.WithTimeout(context.Background(), batonExecTimeout)
	defer cancel()
	hOut, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		log.Printf("[runtime][%s] baton handoff FAILED (degraded, archive only): %v\n%s",
			taskID, err, hOut)
		return
	}
	log.Printf("[runtime][%s] baton handoff ok: %s", taskID, strings.TrimSpace(string(hOut)))
}

// batonProjectDialect — 方言投影公共体（-maka/-acp 同形分叉的收敛，2026-09-22
// 等价简化批）：差异面仅三处——输入旗标名（maka/acp）、输入路径、持笔标识；
// 降级日志以旗标名区分方言。参数与 dsh 链同源（R7 贯通/-agent/幂等重跑）。
func batonProjectDialect(taskID, assignment, flag, inPath, agent string, origin *pb.TaskOrigin) {
	goal := assignmentSummary(assignment)
	if goal == "" {
		return
	}
	bin := batonBin()
	if bin == "" {
		return
	}
	dir := tasklogDir()
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batonExecTimeout)
	defer cancel()
	args := []string{"project",
		"-" + flag, inPath, "-out", filepath.Join(dir, taskID),
		"-task", taskID, "-goal", goal, "-agent", agent}
	if origin != nil {
		if origin.GetUser() != "" {
			args = append(args, "-user", origin.GetUser())
		}
		if origin.GetTenant() != "" {
			args = append(args, "-tenant", origin.GetTenant())
		}
		for _, c := range origin.GetConstraints() {
			if c != "" {
				args = append(args, "-constraint", c)
			}
		}
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		log.Printf("[runtime][%s] baton -%s FAILED (degraded, archive only): %v\n%s",
			taskID, flag, err, out)
		return
	}
	log.Printf("[runtime][%s] baton -%s ok: %s", taskID, flag, strings.TrimSpace(string(out)))
}

// batonProjectTurnMaka — maka 方言投影（-maka）：RuntimeEventLog JSONL（makaSession
// 在沙箱回收前导出的 maka-events.jsonl）→ .tasklog/。金丝雀 park 在 baton 侧落
// .tasklog/PARKED（README §5 纪律）。
func batonProjectTurnMaka(taskID, assignment, makaPath string, origin *pb.TaskOrigin, agent string) {
	// 对抗式审查 P2-4：任务级 agent 优先（换手后接续段 pen 随新 agent），部署级
	// makaAgentID() 兜底（4a/4b 与图路径现状传空）。
	pen := agent
	if pen == "" {
		pen = makaAgentID()
	}
	batonProjectDialect(taskID, assignment, "maka", makaPath, pen, origin)
}

// batonProjectTurnAcp — ACP 方言投影：acpSession 逐回合导出
// 的帧流（acp-events.jsonl JSONL）→ .tasklog/，baton 侧转译复用 maka 模式
// （baton internal/project/acp.go）。
func batonProjectTurnAcp(taskID, assignment, acpPath string, origin *pb.TaskOrigin, agent string) {
	// 对抗式审查 P2-4：同 maka 分叉——任务级 agent 优先，字面量 "acp" 兜底。
	pen := agent
	if pen == "" {
		pen = "acp"
	}
	batonProjectDialect(taskID, assignment, "acp", acpPath, pen, origin)
}

// makaAgentID — maka 投影的持笔标识：descriptor 选择键（runtime.sandbox.agent/
// CODEAUDIT_AGENT，与 LoadDescriptor 同源）——非 agentID() 的 baton 旗标键（两者
// 语义不同：前者选方言，后者标 pen.agent）。空=maka（投影输入即 maka 档案）。
func makaAgentID() string {
	if v, err := codeauditcfg.Default(); err == nil {
		if a, err := v.Str("runtime.sandbox.agent", "CODEAUDIT_AGENT"); err == nil && a != "" {
			return a
		}
	}
	if v := os.Getenv("CODEAUDIT_AGENT"); v != "" {
		return v
	}
	return "maka"
}

// registerBatonFreeze — 注册冻结回调并返回释放函数（R2：进入 gate 挂起时
// 重投影当前 .sse.log + 锚 checkpointed——Run 收束的 defer 投影对 RunSession
// 中途冻结不生效，必须挂起点实时锚定）。goal 取各阶段语义摘要（并行阶段
// 后注册者为准，ADR-238）。
// TP20-T2 切换 marker：PauseAnalysis.agent_to 暂存非空时，冻结回调先向人性化流
// 写「── 切换 Agent from → to ──」（from=当前装配 agent——audit 路径为请求级
// agent，4a/4b/图路径空=注册表首条缺省）；无 agent_to 零 marker（纯暂停，消融
// 口径，计划 §2.4）。
func registerBatonFreeze(p batonPen, goal, projectPath string) (release func()) {
	taskID, aiEntry, agent := p.TaskID, p.Entry, p.Agent // A10b 别名（origin 经 p 透传 batonProjectTurn——本函数体不消费）
	sandbox.OnFrozen(taskID, func() {
		if to := takeSwitchAgent(taskID); to != "" {
			from := agent
			if from == "" {
				if d, err := sandbox.LoadDescriptor(providersPath(), ""); err == nil {
					from = d.ID
				}
			}
			aiEntry.write([]byte(fmt.Sprintf("── 切换 Agent %s → %s ──\n", from, to)))
		}
		batonProjectTurn(p, goal)
		batonHandoffCheckpoint(taskID, projectPath)
	})
	return func() { sandbox.ClearFrozen(taskID) }
}

// 切换目标暂存（TP20-T2）：PauseAnalysis 写入 → 冻结回调消费即清（一次切换一条
// marker；未及消费的暂存随下一次冻结生效——"切换请求挂起至回合边界"语义）。
var (
	switchMu    sync.Mutex
	switchAgent = map[string]string{}
)

func setSwitchAgent(taskID, to string) {
	switchMu.Lock()
	defer switchMu.Unlock()
	switchAgent[taskID] = to
}

func takeSwitchAgent(taskID string) string {
	switchMu.Lock()
	defer switchMu.Unlock()
	to := switchAgent[taskID]
	delete(switchAgent, taskID)
	return to
}

// providersPath — agent_providers.yaml 路径（与 sandbox_analysis.go Run 装配同源
// 派生：codeaudit.yaml 同目录；cfg 不可得=空串=LoadDescriptor 走文件缺失分支）。
func providersPath() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.File()), "agent_providers.yaml")
}

// seedBriefFn — R122 graph-reseed：节点重派种子切片
// 获取器。brief -format dsh -tail 8 渲染脊柱（含机械提炼 lessons）→ ≤1500 rune
// 保尾截断。BATON_BIN 缺失/tasklog 未配置=返回 nil（种子禁用档，诚实降级）；
// 调用失败=WARN 一行+空串（无种子派发，不阻塞执行——⚑2）。
func seedBriefFn(taskID string) func() string {
	bin := batonBin()
	if bin == "" {
		return nil
	}
	dir := tasklogDir()
	if dir == "" {
		return nil
	}
	root := filepath.Join(dir, taskID)
	return func() string {
		ctx, cancel := context.WithTimeout(context.Background(), batonExecTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "brief", "-in", root,
			"-format", "dsh", "-tail", "8").CombinedOutput()
		if err != nil {
			log.Printf("[task %s] baton brief 种子获取失败——无种子派发: %v", taskID, err)
			return ""
		}
		r := []rune(strings.TrimLeft(string(out), "\n"))
		if len(r) > 1500 {
			r = r[len(r)-1500:] // 保尾=保最新
		}
		return string(r)
	}
}
