// HTTP 错误 → 前端行为映射锚定（docs/external-interfaces.md §2 [E-40..E-46]）。
// 错误经 fakeGateway httpError 构造、真实拦截器链回放；503 重试用真实定时器
// （退避 1s/2s/4s 硬编码于 client.ts，是契约本身的一部分）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, clearSession, noteRateLimit, pollIntervalMs } from '../api/client';
import { API_ERROR_EVENT, API_OK_EVENT } from '../api/apiEvents';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {};
const gateway = useFakeGateway(routes);

let apiErrors: number[] = [];
let okCount = 0;
const onError = (e: Event) => apiErrors.push((e as CustomEvent<number>).detail);
const onOk = () => { okCount += 1; };

beforeEach(() => {
  clearSession();
  apiErrors = [];
  okCount = 0;
  window.addEventListener(API_ERROR_EVENT, onError);
  window.addEventListener(API_OK_EVENT, onOk);
});
afterEach(() => {
  window.removeEventListener(API_ERROR_EVENT, onError);
  window.removeEventListener(API_OK_EVENT, onOk);
  vi.restoreAllMocks();
});

describe('E-44 503 自动重试（退避 1s/2s/4s，至多 3 次）', () => {
  it('首次 503 → 重试一次即恢复：共 2 次请求、成功不派发 503 事件', async () => {
    let calls = 0;
    routes['GET /v1/thing'] = () => {
      calls += 1;
      if (calls === 1) httpError(503, { error: 'down' });
      return { ok: true };
    };
    const data = await api.get('/v1/thing');
    expect((data.data as { ok: boolean }).ok).toBe(true);
    const hits = gateway.requests.filter((r) => r.url === '/v1/thing');
    expect(hits).toHaveLength(2);
    expect(apiErrors).toEqual([]);
  }, 15_000);

  it('连续 503 耗尽 3 次重试（共 4 次请求）→ reject + API_ERROR_EVENT(503)', async () => {
    routes['GET /v1/dead'] = () => httpError(503, { error: 'still down' });
    await expect(api.get('/v1/dead')).rejects.toThrow();
    const hits = gateway.requests.filter((r) => r.url === '/v1/dead');
    expect(hits).toHaveLength(4); // 原始 1 + 重试 3（重试上限是契约）
    expect(apiErrors).toEqual([503]); // 耗尽后才降级横幅
  }, 20_000);
});

describe('E-45 403/501 全局事件（auth 端点豁免）', () => {
  it('403 → API_ERROR_EVENT(403)；501 → API_ERROR_EVENT(501)', async () => {
    routes['GET /v1/forbidden'] = () => httpError(403, { error: 'admin only' });
    routes['GET /v1/unsupported'] = () => httpError(501, { error: 'not implemented' });
    await expect(api.get('/v1/forbidden')).rejects.toThrow();
    await expect(api.get('/v1/unsupported')).rejects.toThrow();
    expect(apiErrors).toEqual([403, 501]);
  });

  it('auth 端点 403 不派发事件（登录页自带错误 UX，不出全页覆盖/横幅）', async () => {
    routes['POST /v1/auth/login'] = () => httpError(403, { error: 'forbidden' });
    await expect(api.post('/v1/auth/login', { username: 'u', password: 'p' })).rejects.toThrow();
    expect(apiErrors).toEqual([]);
  });
});

describe('E-40 auth 端点 401 不进刷新链', () => {
  it('401 直接 reject，不尝试 /v1/auth/refresh（防递归），不派发事件', async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    routes['POST /v1/auth/login'] = () => httpError(401, { error: 'bad credentials' });
    await expect(api.post('/v1/auth/login', { username: 'u', password: 'p' })).rejects.toThrow();
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(apiErrors).toEqual([]);
  });
});

describe('E-43 429 退避记录与轮询拉长', () => {
  it('retry_after 记入退避截止：pollIntervalMs 在退避期内拉长、过期后回落', async () => {
    routes['GET /v1/limited'] = () => httpError(429, { error: 'rate limited', retry_after: 7 });
    await expect(api.get('/v1/limited')).rejects.toThrow();
    const stretched = pollIntervalMs(3000);
    expect(stretched).toBeGreaterThan(3000);
    expect(stretched).toBeLessThanOrEqual(7000);
    // 纯 clamp 契约：[5,60]s；缺省 15s
    noteRateLimit(100);
    expect(pollIntervalMs(0)).toBeLessThanOrEqual(60_000);
    noteRateLimit(1);
    expect(pollIntervalMs(0)).toBeLessThanOrEqual(5_000);
    noteRateLimit();
    expect(pollIntervalMs(0)).toBeLessThanOrEqual(15_000);
    expect(pollIntervalMs(0)).toBeGreaterThan(14_000);
  });
});

describe('E-46 任意成功 → API_OK（撤降级横幅）', () => {
  it('2xx 派发 API_OK_EVENT', async () => {
    routes['GET /v1/fine'] = { ok: true };
    await api.get('/v1/fine');
    expect(okCount).toBeGreaterThanOrEqual(1);
  });
});
