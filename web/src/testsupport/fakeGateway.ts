// 共享测试台（ADR-203 测试体系重构）：在 axios adapter 层伪造网关。
// 此前各测试文件 vi.mock 整个 '../api/client' 模块——真实客户端代码被整体绕过，
// mock 响应与后端契约无任何类型/行为关联（ADR-200 改上传响应形状无一测试报红；
// TaskNewPage mock 缺 api 具名导出、错误被 react-query 吞掉，数据从未加载仍全绿）。
// 纪律（.agent/test-gates.md §8）：
//   1. 只在 HTTP 传输层造假——api/client 的真实代码（FormData/序列化/401刷新/503重试）全量执行；
//   2. 未建模路由 = 抛错（响亮失败），禁止静默空成功；
//   3. 错误用 httpError(status, body)——经真实拦截器链（401 刷新/429 退避/503 重试）回放；
//   4. 断言请求形状用本模块返回的 requests 日志，不再自攒 postCalls。
import { afterEach, beforeEach } from 'vitest';
import type { AxiosResponse, InternalAxiosRequestConfig } from 'axios';
import { api } from '../api/client';

export class HttpError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown) {
    super(`HTTP ${status}`);
    this.status = status;
    this.body = body;
  }
}
export function httpError(status: number, body: unknown): never {
  throw new HttpError(status, body);
}

export interface GatewayRequestLog {
  method: string;
  url: string;
  query: string; // 原始 search 串（含 path=… 等）
  body: unknown;
}

export interface HandlerCtx {
  params: Record<string, string>; // ':seg' 路径参数
  query: URLSearchParams;
  body: unknown; // JSON 解析后的请求体（非 JSON 字符串/FormData 原样）
  raw: unknown; // 原始 config.data（FormData 上传时用这个）
  config: InternalAxiosRequestConfig;
}

export type RouteHandler = (ctx: HandlerCtx) => unknown | Promise<unknown>;
// 路由值可以是 handler 函数，也可以直接是响应载荷对象（等价 () => payload）——
// 兼容并行会话（ADR-203）引入的"端点键路由表"简写形态。
type RouteValue = RouteHandler | unknown;

export interface FakeGatewayHandle {
  requests: GatewayRequestLog[];
}

function matchRoute(
  routes: Record<string, RouteValue>,
  method: string,
  path: string,
): { handler: RouteValue; params: Record<string, string> } | null {
  const want = path.split('/').filter(Boolean);
  for (const [key, handler] of Object.entries(routes)) {
    const [m, p] = key.split(/\s+/);
    if (m !== method && m !== '*') continue;
    const segs = p.split('/').filter(Boolean);
    if (segs.length !== want.length) continue;
    const params: Record<string, string> = {};
    let ok = true;
    for (let i = 0; i < segs.length; i++) {
      if (segs[i].startsWith(':')) params[segs[i].slice(1)] = decodeURIComponent(want[i]);
      else if (segs[i] !== want[i]) { ok = false; break; }
    }
    if (ok) return { handler, params };
  }
  return null;
}

// 与 client.ts paramsSerializer 同构（标量照常、对象/数组 JSON 编码），供 handler 读 query
function paramsToSearch(params: Record<string, unknown> | undefined): URLSearchParams {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null) continue;
    sp.append(k, typeof v === 'object' ? JSON.stringify(v) : String(v));
  }
  return sp;
}

function axiosErrorShape(status: number, body: unknown, config: InternalAxiosRequestConfig): Error {
  return Object.assign(new Error(`Request failed with status code ${status}`), {
    config,
    isAxiosError: true,
    response: { status, data: body, headers: {}, config, statusText: String(status) } as AxiosResponse,
  });
}

// 在当前 describe 内注册（beforeEach 挂 adapter，afterEach 卸载）。
// routes 键形如 'GET /v1/projects'、'GET /v1/tasks/:taskId'（':seg' 通配）；'* /path' 匹配任意方法。
export function useFakeGateway(routes: Record<string, RouteValue>): FakeGatewayHandle {
  const requests: GatewayRequestLog[] = [];
  const adapter = async (config: InternalAxiosRequestConfig): Promise<AxiosResponse> => {
    const method = (config.method ?? 'get').toUpperCase();
    const u = new URL(config.url ?? '/', 'http://fake.local');
    const search = paramsToSearch(config.params as Record<string, unknown>);
    for (const [k, v] of new URLSearchParams(u.search)) search.append(k, v);
    const path = u.pathname;
    let body: unknown = config.data;
    if (typeof body === 'string') {
      try { body = JSON.parse(body); } catch { /* 非 JSON，原样 */ }
    }
    requests.push({ method, url: path, query: search.toString(), body });
    const hit = matchRoute(routes, method, path);
    if (!hit) {
      throw new Error(
        `fakeGateway: 未建模路由 ${method} ${path} —— 在 useFakeGateway 补 handler，或确认该链路不应出现（ADR-203 响亮失败纪律）`,
      );
    }
    const { handler: routeValue, params } = hit;
    const handler: RouteHandler = typeof routeValue === 'function' ? (routeValue as RouteHandler) : () => routeValue;
    let data: unknown;
    try {
      data = await handler({ params, query: search, body, raw: config.data, config });
    } catch (e) {
      if (e instanceof HttpError) throw axiosErrorShape(e.status, e.body, config);
      throw e;
    }
    return { data, status: 200, statusText: 'OK', headers: {}, config };
  };
  beforeEach(() => {
    requests.length = 0; // 请求日志按用例隔离（跨用例残留会让"未发生"断言假红）
    (api.defaults as { adapter?: unknown }).adapter = adapter;
  });
  afterEach(() => {
    (api.defaults as { adapter?: unknown }).adapter = undefined;
  });
  return { requests };
}
