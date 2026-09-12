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
package sandbox

import (
	"context"
	"fmt"
)

// defaultLLMEnv — openai 兼容族路由的既有 env 段（ADR-227 前的唯一形态）。
const defaultLLMEnv = "DEEPSEEK_BASE_URL=https://inference.local/v1 DEEPSEEK_API_KEY=openshell-injected"

// anthropicRelayProvider — bridge 在 $DSH_HOME/settings.yaml 烘焙的 llm-pi-ai
// 路由名（dsh-pentest-sse/bridge.mjs 按本约定写入；两侧不可漂移）。
const anthropicRelayProvider = "anthropic-relay"

// llmRouteEnv — 见文件头注释。anthropic 直通路径网关不改写 model 字段（实测
// 2026-09-12：请求什么模型名应答什么），故 DSH_MODEL 必须携带路由真实模型名。
func (r *ManagerRunner) llmRouteEnv(ctx context.Context) string {
	route, err := r.GetInferenceRoute(ctx)
	if err != nil {
		r.event("warn", "推理路由读取失败，沙箱按缺省 deepseek 适配器拉起: %v", err)
		return defaultLLMEnv
	}
	if route.Provider == "" || route.Model == "" {
		return defaultLLMEnv
	}
	p, err := r.GetInferenceProvider(ctx, route.Provider)
	if err != nil {
		r.event("warn", "推理 provider %s 读取失败，沙箱按缺省 deepseek 适配器拉起: %v", route.Provider, err)
		return defaultLLMEnv
	}
	if p.Type != "anthropic" {
		return defaultLLMEnv
	}
	return fmt.Sprintf("DSH_PROVIDER=%s DSH_MODEL=%s ANTHROPIC_API_KEY=openshell-injected",
		anthropicRelayProvider, route.Model)
}
