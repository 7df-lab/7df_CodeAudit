// 共享测试台（ADR-203 测试体系重构）：在 axios adapter 层伪造网关。
// 匹配/回放核心已抽至 mockAdapter.ts（ 2026-09-13，与 dev:mock 走查入口
// 共用）；本文件保留 vitest 生命周期挂卸与测试台既有导出面（HttpError/httpError 等
// re-export，21 个测试文件的既有 import 不动）。
// 纪律（测试门禁）：
//   1. 只在 HTTP 传输层造假——api/client 的真实代码（FormData/序列化/401刷新/503重试）全量执行；
//   2. 未建模路由 = 抛错（响亮失败），禁止静默空成功；
//   3. 错误用 httpError(status, body)——经真实拦截器链（401 刷新/429 退避/503 重试）回放；
//   4. 断言请求形状用本模块返回的 requests 日志，不再自攒 postCalls。
import { afterEach, beforeEach } from 'vitest';
import { api } from '../api/client';
import { buildGatewayAdapter } from './mockAdapter';

export { HttpError, httpError } from './mockAdapter';
export type { GatewayRequestLog, HandlerCtx, RouteHandler, RouteValue } from './mockAdapter';
import type { GatewayRequestLog, RouteValue } from './mockAdapter';

export interface FakeGatewayHandle {
  requests: GatewayRequestLog[];
}

// 在当前 describe 内注册（beforeEach 挂 adapter，afterEach 卸载）。
// routes 键形如 'GET /v1/projects'、'GET /v1/tasks/:taskId'（':seg' 通配）；'* /path' 匹配任意方法。
export function useFakeGateway(routes: Record<string, RouteValue>): FakeGatewayHandle {
  const requests: GatewayRequestLog[] = [];
  const adapter = buildGatewayAdapter(routes, requests);
  beforeEach(() => {
    requests.length = 0; // 请求日志按用例隔离（跨用例残留会让"未发生"断言假红）
    (api.defaults as { adapter?: unknown }).adapter = adapter;
  });
  afterEach(() => {
    (api.defaults as { adapter?: unknown }).adapter = undefined;
  });
  return { requests };
}
