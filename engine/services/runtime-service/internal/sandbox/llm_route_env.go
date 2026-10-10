// llm_route_env.go — bridge 拉起脚本的 LLM env 段按当前推理路由分派（ADR-227）。
//
// anthropic 型路由 → DSH_PROVIDER=anthropic-relay + 路由真实模型名：沙箱 DSH 经
// llm-pi-ai 的 anthropic-messages 适配器打网关 L7 的 /v1/messages（anthropic 协议
// 原生直通，2026-09-12 sim 实测 200 流式/非流式）；凭据由网关按路由携带，沙箱 env
// 只见占位符——凭据不进沙箱纪律不变（与 deepseek 通道同构）。
// 其余（openai 兼容族）→ 既有 deepseek 适配器 env 不变：网关对标准路由请求体改写
// model 字段（cline-consistent-io-chain 研究记载），沙箱缺省模型名即可。
// 路由未设置/读取失败/provider 读取失败 → deepseek env + warn 事件（诚实降级，
// 若 anthropic 路由实际在位而误降级，AI 阶段失败将如实报错，不静默伪装）。
// ADR-257（一期遗留批）：路由 404（工作区未设置）与其他读错误分类入 llmRouteNote——
// 该形态下缺省适配器无有效凭据，LLM 回合必死且 "DeepSeek API error 503" 不可诊断
// （gw-59b8477 等 11 轮误判实录）；note 在回合失败时包装进错误面（可行动修复指引），
// 全局路由回落需跨工作区凭据语义=manager+gateway 立项，engine 不做（ADR-257①）。
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// defaultLLMEnv — openai 兼容族路由的既有 env 段（ADR-227 前的唯一形态）。
const defaultLLMEnv = "DEEPSEEK_BASE_URL=https://inference.local/v1 DEEPSEEK_API_KEY=openshell-injected"

// anthropicRelayProvider — bridge 在 $DSH_HOME/settings.yaml 烘焙的 llm-pi-ai
// 路由名（dsh-pentest-sse/bridge.mjs 按本约定写入；两侧不可漂移）。
const anthropicRelayProvider = "anthropic-relay"

// llmRouteEnv — 见文件头注释。anthropic 直通路径网关不改写 model 字段（实测
// 2026-09-12：请求什么模型名应答什么），故 DSH_MODEL 必须携带路由真实模型名。
func (r *ManagerRunner) llmRouteEnv(ctx context.Context) string {
	return r.llmRouteEnvFromRoute(ctx, r.inferenceRouteSnapshot(ctx))
}

// inferenceRouteSnapshot — M1 挂载与 M2 env 分派共用的路由单快照（code-review P2#2：
// 消除双读一致性窗口——两次 GET 间隔 create+wait-ready 秒级，切路由瞬间的在途
// launch 会拿到"合成层=旧 provider、直连 env=新 provider"的撕裂态，新端点不在
// 合成层内被 fail-closed 拒）。读失败/空路由 → nil+warn（挂载跳过+env 走缺省降级，
// 不阻断创建——挂载为渐进激活：无 profile 的 provider 网关侧跳过合成层，行为与
// 现状一致）。
func (r *ManagerRunner) inferenceRouteSnapshot(ctx context.Context) *InferenceRouteState {
	route, err := r.GetInferenceRoute(ctx)
	if err != nil {
		// ADR-257②: 404=工作区未注册路由（可行动——manager 注册即可修）；其他=读取故障。
		// note 进 llmRouteNote，回合失败时包装进错误面（wrapTurnErrWithRouteNote）。
		var httpErr *ManagerHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 404 {
			r.llmRouteNote = fmt.Sprintf("工作区 %q 未注册推理路由（manager 404）——沙箱按缺省适配器拉起，其无有效凭据，LLM 回合预计失败；修复：在 manager 为该工作区注册 provider 并设置 route（PUT /api/v1/inference/providers + /route，workspace=%s）",
				r.cfg.Workspace, r.cfg.Workspace)
			r.event("warn", "%s", r.llmRouteNote)
		} else {
			r.llmRouteNote = fmt.Sprintf("推理路由读取失败: %v", err)
			r.event("warn", "推理路由读取失败（spec.providers 挂载与 LLM env 均按缺省形态）: %v", err)
		}
		return nil
	}
	return route
}

// wrapTurnErrWithRouteNote — ADR-257②：回合失败错误穿透路由缺失上下文（纯函数）。
// 空 note=原样返回（路由正常时不包装）；空 err=原样返回。落点=task error_message 与
// 降级链 emit 行同源（sandboxErrHint 取 err.Error()），GUI 用户直接看到可行动原因。
func wrapTurnErrWithRouteNote(note string, err error) error {
	if note == "" || err == nil {
		return err
	}
	return fmt.Errorf("推理路由缺失（%s）: %w", note, err)
}

func (r *ManagerRunner) llmRouteEnvFromRoute(ctx context.Context, route *InferenceRouteState) string {
	if route == nil || route.Provider == "" || route.Model == "" {
		return defaultLLMEnv
	}
	p, err := r.GetInferenceProvider(ctx, route.Provider)
	if err != nil {
		r.event("warn", "推理 provider %s 读取失败，沙箱按缺省 deepseek 适配器拉起: %v", route.Provider, err)
		return defaultLLMEnv
	}
	// M2 直连形态（provider attachment 设计，契约 m1m2-impl-contract §2）：开关开时
	// base_url 换 provider 真实端点、假凭据退役（supervisor 占位符 env 透出——键名
	// 契约=provider credentials 键与客户端读键同名）。前置依赖 M1 的 spec.providers
	// 挂载（端点合成层）；base_url 缺失=warn 后按现状形态继续（该类型原分支，非一律
	// deepseek 缺省——anthropic 型现状=injected 形态）。
	if r.cfg.LLMDirectEgress {
		if env, ok := r.directEgressEnv(route, p); ok {
			r.event("info", "直连形态激活 provider=%s type=%s", route.Provider, p.Type)
			return env
		}
	}
	// R80: EqualFold 对齐写入口径（service 层 rejectInferenceAliasKeys 用 EqualFold）——
	// 大小写漂移曾令 "Anthropic" 型静默走 deepseek 通道且无 warn。
	if !strings.EqualFold(p.Type, "anthropic") {
		return defaultLLMEnv
	}
	return fmt.Sprintf("DSH_PROVIDER=%s DSH_MODEL=%s ANTHROPIC_API_KEY=openshell-injected",
		anthropicRelayProvider, shQuoteLite(route.Model))
}

// directEgressEnv — M2 直连形态 env 段。base_url 从 provider config 读（引擎白名单键：
// anthropic 型=ANTHROPIC_BASE_URL，openai 族=OPENAI_BASE_URL——实测 provider config
// 不随网关凭据快照下发，真实值必须此处注入）。@ai-sdk 语义 anthropic 面 base_url 须
// 含 /v1（Maka S2 翻案根因①），写入口径已在 manager 侧校验，此处不重复。
// 凭据 env 不注：占位符由 supervisor 按 provider credentials 键注入 guest（键名契约：
// anthropic=ANTHROPIC_API_KEY、openai 族=DEEPSEEK_API_KEY——bridge/llm-deepseek 读键）。
func (r *ManagerRunner) directEgressEnv(route *InferenceRouteState, p *InferenceProvider) (string, bool) {
	isAnthropic := strings.EqualFold(p.Type, "anthropic")
	baseURL := p.Config["OPENAI_BASE_URL"]
	if isAnthropic {
		baseURL = p.Config["ANTHROPIC_BASE_URL"]
	}
	if baseURL == "" {
		r.event("warn", "直连形态开启但 provider %s 的 config 缺 base_url（%s），回落该类型现状形态（legacy）",
			route.Provider, map[bool]string{true: "ANTHROPIC_BASE_URL", false: "OPENAI_BASE_URL"}[isAnthropic])
		return "", false
	}
	if isAnthropic {
		return fmt.Sprintf("DSH_PROVIDER=%s DSH_MODEL=%s ANTHROPIC_BASE_URL=%s",
			anthropicRelayProvider, shQuoteLite(route.Model), shQuoteLite(baseURL)), true
	}
	// DSH_MODEL 必须显式携带：legacy 形态网关 router 改写请求体 model 字段（cline
	// 研究记载），直连形态无网关改写——缺省模型名（bridge 缺省 deepseek-v4-flash）
	// 会被真实端点拒（e2e 实证"模型不存在"→降级 RuleScan）。
	return fmt.Sprintf("DSH_MODEL=%s DEEPSEEK_BASE_URL=%s",
		shQuoteLite(route.Model), shQuoteLite(baseURL)), true
}

// shQuoteLite — R80 兜底：单引号包裹+内嵌单引号转义。白名单在写入口（service 层），
// 读路径对存量/绕过网关直写 manager 的路由值不裸拼进 bash -c。
func shQuoteLite(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
