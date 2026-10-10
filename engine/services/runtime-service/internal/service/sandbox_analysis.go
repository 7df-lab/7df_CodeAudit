// 沙箱语义分析接线（ADR-140/ADR-166）：模式A RunAIAnalysis 与模式D ReviewSASTResults 的
// LLM 语义环节经 openshell-manager 微服务在沙箱内执行 DSH；不可用（mode=off/manager
// 不可达/执行失败）时回退既有降级链（RuleScan / NEEDS_MANUAL，07 §10），回退原因留日志。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// sandboxCfg — 从全局配置装配沙箱执行器（缺键 fail-fast，ADR-137）。
func sandboxCfg() (*sandbox.Config, error) {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return nil, err
	}
	mode, err := cfg.Str("runtime.sandbox.mode")
	if err != nil {
		return nil, err
	}
	mgrURL, err := cfg.Str("runtime.sandbox.manager_url")
	if err != nil {
		return nil, err
	}
	mgrToken, err := cfg.Str("runtime.sandbox.manager_token")
	if err != nil {
		return nil, err
	}
	workspace, err := cfg.Str("runtime.sandbox.workspace")
	if err != nil {
		return nil, err
	}
	image, err := cfg.Str("runtime.sandbox.image")
	if err != nil {
		return nil, err
	}
	waitS, err := cfg.Int("runtime.sandbox.wait_ready_timeout_s")
	if err != nil {
		return nil, err
	}
	// R76: exec_timeout_s 死配置清除（ExecTimeoutS 装配后零消费）；改装配 HTTP 兜底
	// 超时（挂起保险，缺省 2100s>07 §8:126 会话 30m 上界；宽松读取，缺键不 fail-fast）。
	httpClientTimeoutS := 2100
	if v, err := cfg.Int("runtime.sandbox.http_client_timeout_s", "CODEAUDIT_DSH_HTTP_CLIENT_TIMEOUT_S"); err == nil && v > 0 {
		httpClientTimeoutS = v
	}
	maxTok, err := cfg.Int("runtime.sandbox.dsh_max_tokens")
	if err != nil {
		return nil, err
	}
	// env 覆盖（2026-09-05 生产脚本需要：yaml 硬编码现役实例 IP，任意服务器部署时
	// 经 CODEAUDIT_GATEWAY_DIAL_ADDR 注入本机网关拨号地址；空值语义不变=由 manager 推导）
	gatewayDial, err := cfg.Str("runtime.sandbox.gateway_dial_addr", "CODEAUDIT_GATEWAY_DIAL_ADDR")
	if err != nil {
		return nil, err
	}
	// R6 首批（TP17-T4）：bridge 拉起命令 cfg 驱动（空=默认 DSH bridge 常量）；
	// descriptor 化后为双轨 legacy 键（优先级低于条目值）。
	agentBridge := ""
	if v, err := cfg.Str("runtime.sandbox.agent_bridge_cmd", "CODEAUDIT_AGENT_BRIDGE_CMD"); err == nil {
		agentBridge = v
	}
	// R6 完整版 v0（切片 1）：descriptor 条目装配——configs/agent_providers.yaml（设计 §2
	// "或独立 providers.yaml"承载，go-config 无对象数组 API）；文件缺失=合成缺省 DSH 条目
	//（现网零改动兼容），文件存在但非法=fail-fast（诚实，不静默回落）。依据: 主仓库
	// docs/designs/platform-slice1-r6-descriptor.md §2/§6；ADR-244。
	agentID := ""
	if v, err := cfg.Str("runtime.sandbox.agent", "CODEAUDIT_AGENT"); err == nil {
		agentID = v
	}
	providersPath := filepath.Join(filepath.Dir(cfg.File()), "agent_providers.yaml")
	agent, err := sandbox.LoadDescriptor(providersPath, agentID)
	if err != nil {
		return nil, err
	}
	// ADR-248：targeted 组合模板路径（宽松读取——restricted 缺省档无需模板，
	// 文件缺失在 targeted 组合时 fail-closed 报出，不在装配期拦死）。
	policyTmpl := "/etc/codeaudit/policy/targeted.yaml"
	if v, err := cfg.Str("runtime.sandbox.policy_template", "CODEAUDIT_POLICY_TEMPLATE"); err == nil && v != "" {
		policyTmpl = v
	}
	// M2 provider attachment：直连形态开关（缺省 false=现状 inference.local 形态，
	// 上线零行为变化；true 需路由 provider 已 profile 化——契约 m1m2-impl-contract §2）。
	// 装配期无日志/事件出口，读取错误（env 拼错/yaml 非 bool）静默 false——fail-safe
	// 方向（P3#5 code-review：与 mode/mgrURL 的 fail-fast 风格差异系装配面取舍）。
	llmDirectEgress := false
	if v, err := cfg.Bool("runtime.sandbox.llm_direct_egress", "CODEAUDIT_LLM_DIRECT_EGRESS"); err == nil {
		llmDirectEgress = v
	}
	return &sandbox.Config{
		Mode: mode, ManagerURL: mgrURL, ManagerToken: mgrToken,
		Workspace: workspace, Image: image,
		PolicyTemplate:    policyTmpl,
		LLMDirectEgress:   llmDirectEgress,
		WaitReadyTimeoutS: waitS,
		HTTPClientTimeout: time.Duration(httpClientTimeoutS) * time.Second,
		DSHMaxTokens:      maxTok, GatewayDialAddr: gatewayDial,
		AgentBridgeCmd: agentBridge,
		Agent:          agent,
		ProvidersPath:  providersPath,
	}, nil
}

// interactionDir — AI 交互日志落盘根目录（ADR-168；读失败=空 → 仅内存留存）。
// CODEAUDIT_INTERACTION_DIR 部署覆盖口：容器化后 task-service 对账探针（ADR-196）
// 与本服务须指向同一物理目录（相对路径在各容器 CWD 下互不可见，gw-d331089f 实证）。
func interactionDir() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return ""
	}
	v, err := cfg.Str("runtime.sandbox.interaction_dir", "CODEAUDIT_INTERACTION_DIR")
	if err != nil {
		return ""
	}
	return v
}

// applyWorkspaceOverride — ADR-247（切片3后半）：TaskOrigin.workspace 非空覆盖
// 全局缺省——per-team 命名空间贯穿单点（沙箱归属+manager inference 寻址同源
// cfg.Workspace）。空串=回落 runtime.sandbox.workspace（既有部署零改动）。
// engine 只传寻址值不执法：workspace 合法性/隔离执法者唯一在网关（公理 B1）。
func applyWorkspaceOverride(cfg *sandbox.Config, origin *pb.TaskOrigin) {
	if origin == nil {
		return
	}
	if origin.GetWorkspace() != "" {
		cfg.Workspace = origin.GetWorkspace()
	}
	// ADR-248（切片4后半）：网络分型随任务贯穿——profile 源=pack spec 创建时
	// 快照（空=restricted 缺省档）；授权目标=任务【资源】字段快照。组合发生在
	// launch（sandboxSpecWithPolicy 唯一构造点）。
	cfg.NetworkProfile = origin.GetNetworkProfile()
	for _, t := range origin.GetAuthorizedTargets() {
		cfg.AuthorizedTargets = append(cfg.AuthorizedTargets,
			sandbox.TargetEndpoint(t.GetHost(), t.GetPort(), t.GetAllowedIps()))
	}
}

// analyzeViaSandbox — 沙箱内 DSH 语义分析；返回 (findings, nil) 或 (nil, err)。
// 调用方收到 err 时回退降级链。reasoning 统一加 [DSH-sandbox] 前缀（溯源标记）。
// emit 可空：沙箱生命周期事件出口（GUI 执行日志，ADR-167）。
func analyzeViaSandbox(ctx context.Context, taskID, projectPath, assignment string, emit TaskLogFunc, origin *pb.TaskOrigin, agentID, requestID string) (findings []*pb.UnifiedFinding, storedIDs []string, err error) {
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, nil, err
	}
	applyWorkspaceOverride(cfg, origin) // ADR-247: per-team workspace 贯穿
	// 切片 7：任务级 Agent 覆盖（空=no-op 字节等价；非法 id=诚实失败）
	log.Printf("[slice7-diag] analyzeViaSandbox taskID=%s agentID=%q cfg.Agent=%s providers=%s", taskID, agentID, cfg.Agent.ID, cfg.ProvidersPath)
	if err := sandbox.SelectAgent(cfg, agentID); err != nil {
		log.Printf("[slice7-diag2] SelectAgent err: %v", err)
		return nil, nil, err
	}
	log.Printf("[slice7-diag2] after SelectAgent cfg.Agent=%s dialect=%s image=%q", cfg.Agent.ID, cfg.Agent.Dialect, cfg.Agent.Launch.SandboxImage)
	if emit != nil {
		cfg.EventFn = func(level, msg string) { emit(level, msg) } // source=sandbox 由 taskLogSink 标定
	}
	// AI 交互日志（ADR-168 补遗）：人性化流→内存（GetAIInteractionLog 面向用户）+ .ai.log
	// 落盘；原始 SSE 帧→仅 .sse.log 落盘（机器调试留存，不经 RPC）。终态定格。落盘失败
	// 静默——留存通道不得影响分析主链路（07 §10 降级纪律同源）。
	// ADR-215: 条目创建与回调接线统一走 wireAILog（回调必须先于 runner 构造）
	aiEntry := wireAILog(cfg, taskID)
	r := sandbox.NewManagerRunner(*cfg)
	if aiEntry == nil {
		return nil, nil, sandbox.ErrDisabled
	}
	// R1 投影钩子（TP17-T1）：回合收束（含错误路径）后 .sse.log→baton→.tasklog/。
	// defer LIFO：先注册者后执行——本行须在 finish 之前注册，保证投影发生在收束之后。
	// 依据: 主仓库 baton/docs/engine-integration-req.md R1 / ADR-237
	defer batonProjectTurn(batonPen{TaskID: taskID, Entry: aiEntry, Origin: origin, Agent: agentID}, assignment) // D2：pen 随任务级 agent（换手后接续段投影正确）
	defer aiEntry.finish()
	// R2 冻结回调（TP17-T2）：RunSession 中途冻结时实时重投影+锚基线（本函数的
	// Run 单回合无 gate，此注册覆盖同任务后续 RunSession 阶段冻结——4a/4b 接线
	// 见 sandbox_verify.go）。依据: engine-integration-req R2 / ADR-238
	releaseFreeze := registerBatonFreeze(batonPen{TaskID: taskID, Entry: aiEntry, Origin: origin, Agent: agentID}, assignmentSummary(assignment), projectPath)
	defer releaseFreeze()
	// TP20-T2 用例⑤：接续简报段——多纪元
	// 任务（切换 handoff-retry/auto-retry 重入）首回合 prompt 前置 baton 前情切片：
	// 上下文重建=档案单文件连续（D2/S2）+prompt 面显式接续双轨。brief 获取器与
	// R122 graph-reseed 同源（seedBriefFn）；冷启动档案空/brief 空=无段（消融口径）；
	// BATON_BIN/tasklog 未配置=禁用档、失败=WARN+无段不阻塞（⚑2 同纪律）。
	if briefFn := seedBriefFn(taskID); briefFn != nil {
		assignment = assignmentWithBrief(assignment, briefFn())
	}
	res, err := r.Run(ctx, sandbox.Task{
		TaskID:       taskID,
		WorkspaceDir: projectPath,
		Assignment:   assignment,
		TasklogDir:   tasklogDir(), // maka 方言 RuntimeEventLog 导出位（空=禁用，诚实降级）
		Timeout:      0,            // 07 §8:117/ADR-191：模式 B/C 不设外层时限（R76 前为死值 10m，接线即预杀健康回合）；挂起由 HTTPClientTimeout 兜底
	})
	if err != nil {
		return nil, nil, err
	}
	// 复批 II（附录 C2）：检测结果**回合收束即落库**（原在 RunAIAnalysis 尾——
	// 任务终态才见）。Quality 打标随迁（数据面提前；completeAgentPhase 阶段推进
	// 仍留 pipeline 原位）；幂等键=requestID+"-store" 与原键同（迁移不改幂等语义）。
	mapped := mapSandboxFindings(taskID, projectPath, res.Findings)
	suspicious := 0
	for _, f := range mapped {
		p := strings.ToLower(f.GetLocation().GetFilePath())
		if strings.Contains(p, "_test") || strings.Contains(p, "/test") || strings.Contains(p, "sample") || strings.Contains(p, "mock") {
			f.VerdictDirection = pb.VerdictDirection_VERDICT_DIRECTION_UNSPECIFIED // 质量门拒判=未判定，provenance 在 AiReasoning
			f.AiReasoning = "[降级] quality-validator: located under test/sample path; LLM cross-validation unavailable — manual review required"
			suspicious++
		}
	}
	if suspicious > 0 {
		log.Printf("[runtime][%s] Quality Validator: %d test/sample-path hits to NEEDS_MANUAL", taskID, suspicious)
	}
	if rAddr := cfgAddr("result", "CODEAUDIT_RESULT_ADDR"); len(mapped) > 0 && rAddr != "" && requestID != "" {
		var serr error
		storedIDs, serr = batchCreateFindingsToResult(rAddr, taskID, requestID+"-store", mapped)
		if serr != nil {
			return nil, nil, fmt.Errorf("persist %d ai findings to result-service: %w", len(mapped), serr)
		}
		if emit != nil {
			emit("info", fmt.Sprintf("检测回合收束：findings=%d 已实时落库（发现列表可查）", len(storedIDs)))
		}
	}
	// 两段式补丁管线：
	// 一阶段=上方主回合（assignmentTemplate 纯检测，diff_patch 恒空）；二阶段=对所有
	// 确认发现统一生成补丁（清单含 fix_suggestion 作意图输入）；三阶段=fixretry 失败
	// 反馈自纠（触发条件不变）。同一沙箱 runner 各自独立回合（每 Run 独立沙箱生命周期）；
	// patchRoundFn 闭包二/三阶段复用。gw-d331089f（R34）：PatchFixRound=true 让 Run 按
	// patches 语义解析——套用 findings 解析会把合规 submit_patches 提交整轮判废。
	patchRound := func(ctx context.Context, fixAssignment string) ([]sandbox.PatchFix, error) {
		fixRes, fixErr := r.Run(ctx, sandbox.Task{
			TaskID:        taskID + "-fix",
			WorkspaceDir:  projectPath,
			Assignment:    fixAssignment,
			Timeout:       0, // 07 §8:117/ADR-191：同主回合不设外层时限（R76）
			PatchFixRound: true,
		})
		if fixErr != nil {
			return nil, fixErr
		}
		return fixRes.Patches, nil
	}
	// 二阶段：补丁生成（零发现时 applyPatchPhase 内零开销直返，不发回合、不打阶段行）。
	// 阶段行进任务日志（GUI 执行日志可见；UI 阶段时间线不新增阶段，仍归 AI 推理）。
	// R97 可见性补遗（gw-94567173530a 报障"提交风险后页面不实时显示"）：findings 落库
	// 在 task-service 编排（RunAIAnalysis RPC 返回后），AI 阶段中途无法进发现列表——
	// 最低成本的第一档可见性=一阶段收束即把发现摘要上浮到实时执行日志（标题清单），
	// 二阶段完成后的发现列表实时化（中途落库/流式事件）属架构项另立。
	if emit != nil && len(res.Findings) > 0 {
		titles := make([]string, 0, len(res.Findings))
		for i, f := range res.Findings {
			if i == 5 {
				titles = append(titles, fmt.Sprintf("…等共 %d 项", len(res.Findings)))
				break
			}
			titles = append(titles, f.Title)
		}
		// 文案中性化（对抗审查 R97 补遗）：本函数被模式A（检测）与模式D（SAST 复核）
		// 共用——"一阶段检测完成"对复核语境标签错位，"沙箱产出"两模式皆准确。
		emit("info", fmt.Sprintf("沙箱产出 %d 项发现——%s", len(res.Findings), strings.Join(titles, "；")))
		emit("info", fmt.Sprintf("进入补丁生成阶段：%d 项", len(res.Findings)))
	}
	res.Findings = applyPatchPhase(ctx, projectPath, res.Findings, emit, patchRound)
	// 三阶段：非空但校验失败的补丁走一轮失败反馈再生成（Cline 式自纠，失败详情含
	// 相似度+上下文预览喂回模型）；二阶段已过滤后通常零开销（无失败项不发回合）。
	res.Findings = retryFailedPatches(ctx, taskID, projectPath, res.Findings, emit, patchRound)
	return mapped, storedIDs, nil
}

// assignmentWithBrief — TP20-T2 用例⑤：非空 brief 前置接续段（段头文案=golden 锚，
// 与 graph.go「前任失败要点」段同型异义——彼为失败教训此为进度前情）；空/纯空白=
// 原样返回（冷启动消融：首轮档案空无前情段）。纯函数便于 golden 锁定。
func assignmentWithBrief(assignment, brief string) string {
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return assignment
	}
	return "## 前情简报（接续——前序回合档案切片，先核对再行动）\n" + brief + "\n\n" + assignment
}

// mapSandboxFindings — 沙箱发现 → proto UnifiedFinding。
// (TRUE, LIKELY)：DSH 报告即模型断言"这是真实漏洞"，但自报未验证不越权 DEFINITE 档
// （那是对齐人工确认的口径；V1 契约 AI/人工共用字段，语义靠 reasoning 前缀区分）。
// SourceRaw：捕获发现位置 ±10 行真实文件内容（对齐 ADR-143 适配器同款 schema，
// GUI 代码上下文卡片直接渲染；读取失败如实留空——不编造上下文）。
// ADR-183：AiFixSuggestion（LLM markdown 原文）+ DiffPatch（经 fixpatch 服务端校验重建，
// 失配丢弃置空——finding 保留，不编造补丁）。
// unlocatedFilePlaceholder — 未定位发现的 FilePath 占位（复批 II C3 引入；R126 起
// dedupKey 同认此占位为未定位态——占位保展示、去重认占位，两面不得各写各的字面量）。
const unlocatedFilePlaceholder = "（未定位）"

func mapSandboxFindings(taskID, projectPath string, findings []sandbox.Finding) []*pb.UnifiedFinding {
	out := make([]*pb.UnifiedFinding, 0, len(findings))
	for _, f := range findings {
		// 复批 II C3：text-final 字段漂移兜底（sim gw-85011e 实证 file_path 空导致
		// 同空 dedup 键 3→1 挤压+定位缺失）——FilePath 空给显式占位，保留发现不丢。
		if strings.TrimSpace(f.FilePath) == "" {
			f.FilePath = unlocatedFilePlaceholder
		}
		out = append(out, &pb.UnifiedFinding{
			FindingId:        "", // R135：内容稳定 ID 构造完回填（占位修正后 dedupKey 摘要）
			TaskId:           taskID,
			SourceTool:       "ai_agent", // 04 §3.1 阶段3 产物口径
			SourceRuleId:     "dsh-headless",
			Title:            f.Title,
			Description:      f.Description,
			Severity:         pb.Severity(pb.Severity_value[f.Severity]),
			CweId:            f.CweID,
			Confidence:       float32(f.Confidence),
			VerdictDirection: pb.VerdictDirection_VERDICT_DIRECTION_TRUE,
			VerdictTier:      pb.VerdictTier_VERDICT_TIER_LIKELY, // 自报未验证=可能档，不越权 DEFINITE
			AiConfidence:     float32(f.Confidence),
			AiReasoning:      "[DSH-sandbox] " + f.Reasoning,
			AiFixSuggestion:  f.FixSuggestion,
			DiffPatch:        validatedDiffPatch(taskID, f.DiffPatch, projectPath),
			Location:         &pb.LocationInfo{FilePath: f.FilePath, StartLine: int32(f.StartLine)},
			SourceRaw:        captureCodeContext(projectPath, f.FilePath, int(f.StartLine)),
		})
		// R135：内容稳定 ID=dedupKey 摘要——原序号
		// taskID-sbx-N 每纪元从 1 重编，epoch-2 重报同发现=主键碰撞全败（result
		// 日志 22×duplicate key 实证）；内容 ID+落库面 ON CONFLICT 幂等=重报 no-op、
		// 纪元新发现独立入库。
		cand := out[len(out)-1]
		cand.FindingId = fmt.Sprintf("%s-sbx-%s", taskID, digest12(dedupKey(cand))) // R135：内容稳定 ID
		// F5：跨纪元近重复治理——行号漂移的同漏洞=不同稳定 ID
		// （dedupKey 含 line）各行入库；DedupGroup=无行号摘要（file:cwe；未定位回落
		// dedupKey 本身即无行号）供融合视图分组（FusionView 按 dedup_group 归组）。
		cand.DedupGroup = fmt.Sprintf("%s-dg-%s", taskID, digest12(dedupGroupKey(cand)))
	}
	return out
}

// dedupGroupKey — F5：无行号分组键（file:cwe；未定位=dedupKey 同源无行号）。
func dedupGroupKey(f *pb.UnifiedFinding) string {
	if fp := f.GetLocation().GetFilePath(); fp == "" || fp == unlocatedFilePlaceholder {
		if f.GetLocation().GetStartLine() == 0 {
			return dedupKey(f) // 未定位回落本就无行号
		}
	}
	return fmt.Sprintf("%s:%s", f.GetLocation().GetFilePath(), f.GetCweId())
}

// digest12 — R135：dedupKey 的 sha256 前 12 hex（内容稳定 ID 摘要面）。
func digest12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// codeCtxRadius — 代码上下文捕获半径（与 ADR-143 适配器 ±10 行同口径）。
const codeCtxRadius = 10

// captureCodeContext — 从项目文件读取发现位置 ±10 行，产出 ADR-143 source_raw schema
// 的原始 JSON 字节（bytes 字段；base64 由 protojson HTTP 序列化层完成）。
// 文件不可读/越界如实返回空——不编造上下文。
func captureCodeContext(projectPath, filePath string, startLine int) []byte {
	if projectPath == "" || filePath == "" || startLine <= 0 {
		return nil
	}
	abs := filepath.Join(projectPath, filepath.Clean("/"+filePath)) // 清洗防穿越
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if startLine > len(lines) {
		return nil
	}
	s := startLine - codeCtxRadius
	if s < 1 {
		s = 1
	}
	e := startLine + codeCtxRadius
	if e > len(lines) {
		e = len(lines)
	}
	window := make([]string, 0, e-s+1)
	for i := s; i <= e; i++ {
		window = append(window, lines[i-1])
	}
	payload, err := json.Marshal(map[string]any{
		"code": lines[startLine-1],
		"line": startLine,
		"context": map[string]any{
			"start_line": s, "end_line": e, "lines": window,
		},
	})
	if err != nil {
		return nil
	}
	return payload
}

// sandboxAssignmentModeA — 模式A 语义分析任务指令（04 §3.1 阶段3 语义）。
func sandboxAssignmentModeA() string {
	return "对项目做安全审计：识别可被利用的漏洞（注入/鉴权缺失/敏感信息/不安全反序列化等），" +
		"逐条给出代码证据与判定理由；按输出契约返回 findings。"
}

// sandboxAssignmentReview — 模式D 逐条审核任务指令（04 §3.4 步骤2 口径）。
func sandboxAssignmentReview(findingsJSON string) string {
	return "以下是 SAST 工具的发现清单（JSON）。逐条交叉验证（读代码证据→数据流→可达性），" +
		"对每条给出 verdict（TRUE_POSITIVE/FALSE_POSITIVE/NEEDS_MANUAL）与理由；" +
		"同时补充清单遗漏的真实漏洞到 findings。按输出契约返回：" +
		`{"findings": [...], "reviews": [{"finding_id":"...","verdict":"...","reasoning":"..."}]}` +
		"\n\n发现清单：\n" + findingsJSON
}

// sandboxErrHint — 降级日志用：错误摘要（去换行）。
func sandboxErrHint(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", " ")
}

// sandboxReview — 模式D 沙箱逐条审核：一次沙箱运行完成全部交叉验证。
// 返回 (reviews, opinionCount, err)；ErrDisabled/失败由调用方回退降级链。
func sandboxReview(ctx context.Context, taskID, projectPath string, findings []*pb.UnifiedFinding, emit TaskLogFunc, origin *pb.TaskOrigin, agentID, requestID string) (
	[]*pb.FindingReview, map[string]int32, error) {
	if len(findings) == 0 {
		return nil, nil, fmt.Errorf("no findings to review")
	}
	list := make([]map[string]interface{}, 0, len(findings))
	for _, f := range findings {
		list = append(list, map[string]interface{}{
			"finding_id": f.GetFindingId(), "title": f.GetTitle(), "cwe_id": f.GetCweId(),
			"severity": f.GetSeverity().String(), "file": f.GetLocation().GetFilePath(),
			"line": f.GetLocation().GetStartLine(),
		})
	}
	raw, _ := json.Marshal(list)
	// ADR-212: project_path 由请求贯通（同包 SearchMissedVulns 在 ADR-165 已判
	// "无人设置的环境变量"为断线缺陷）——env 仅作旧口径兼容回落
	if projectPath == "" {
		projectPath = os.Getenv("CODEAUDIT_PROJECT_REPO_PATH")
	}
	res, _, err := analyzeViaSandbox(ctx, taskID, projectPath,
		sandboxAssignmentReview(string(raw)), emit, origin, agentID, requestID)
	if err != nil {
		return nil, nil, err
	}
	// 从沙箱原始 stdout 拿 reviews（analyzeViaSandbox 只映射了 findings）——
	// 简化：reviews 由 runner envelope 无法承载，故此处要求沙箱输出 reviews 字段时
	// 通过 ai_reasoning 回填 verdict 映射（reviews 由 [DSH-sandbox] 前缀 reasoning 的
	// findings 与原清单 dedupKey 匹配生成）。
	reviews := make([]*pb.FindingReview, 0, len(findings))
	opinions := map[string]int32{}
	byKey := map[string]*pb.UnifiedFinding{}
	for _, f := range res {
		byKey[dedupKey(f)] = f
	}
	for _, f := range findings {
		opinion := pb.ReviewOpinion_REVIEW_OPINION_SUGGEST_REVIEW // 缺省=建议复核（未判定/可能档同此）
		if m, ok := byKey[dedupKey(f)]; ok && m.GetAiConfidence() > 0 {
			// 意见映射按方向：判真→确认，判假→否决；
			// 未判定→建议复核。档位不改意见（"可能为真"也应优先人工看一眼）
			switch m.GetVerdictDirection() {
			case pb.VerdictDirection_VERDICT_DIRECTION_TRUE:
				opinion = pb.ReviewOpinion_REVIEW_OPINION_CONFIRM
			case pb.VerdictDirection_VERDICT_DIRECTION_FALSE:
				opinion = pb.ReviewOpinion_REVIEW_OPINION_REJECT
			}
		}
		opinions[opinion.String()]++
		reasoning := "sandbox DSH cross-validation unavailable for this item"
		if m, ok := byKey[dedupKey(f)]; ok && m.GetAiReasoning() != "" {
			reasoning = m.GetAiReasoning()
		}
		reviews = append(reviews, &pb.FindingReview{
			FindingId:  f.GetFindingId(),
			Opinion:    opinion,
			Confidence: 0.5,
			Reasoning:  reasoning,
		})
	}
	return reviews, opinions, nil
}
