// @vitest-environment node
// 401 单飞刷新回归（14号 §2.1）：并发 401 只触发一次刷新。
// 用 axios 自定义 adapter 模拟后端（官方支持口径，无 worker 序列化噪声）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AxiosRequestConfig, AxiosResponse, InternalAxiosRequestConfig } from 'axios';
import { api, clearSession, saveRefreshToken, setAccessToken, readRefreshToken } from '../api/client';

const store = new Map<string, string>();
// node 环境无 localStorage：client.ts 经 globalThis.localStorage 可选访问，这里注入内存实现
Object.defineProperty(globalThis, 'localStorage', {
  configurable: true,
  get: () => ({
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  }),
});

let refreshCalls = 0;
let accessSeen = '';

// fetch 直连打桩：仅 /v1/auth/refresh 会经过它（requestRefresh 用裸 fetch）
const fetchStub = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const url = String(input);
  if (url.endsWith('/v1/auth/refresh')) {
    refreshCalls += 1;
    if (readRefreshToken() === 'expired') {
      return new Response('{"error":"expired"}', { status: 401 });
    }
    return new Response(JSON.stringify({ access_token: 'new-access', refresh_token: 'refresh-2', expires_in_s: 1800 }),
      { status: 200, headers: { 'Content-Type': 'application/json' } });
  }
  throw new Error('unexpected fetch: ' + url);
};

// 自定义 adapter：校验 Authorization 头，401/200 行为可控
const mockAdapter = async (config: InternalAxiosRequestConfig): Promise<AxiosResponse> => {
  const auth = (config.headers?.Authorization as string) ?? '';
  accessSeen = auth;
  if (auth !== 'Bearer new-access') {
    throw Object.assign(new Error('Request failed with status code 401'), {
      config, isAxiosError: true,
      response: { status: 401, data: {}, headers: {}, config, statusText: 'Unauthorized' } as AxiosResponse,
    });
  }
  return { data: { ok: true }, status: 200, statusText: 'OK', headers: {}, config };
};

describe('api client 401 刷新', () => {
  beforeEach(() => {
    store.clear();
    clearSession();
    refreshCalls = 0;
    (api.defaults as { adapter?: unknown }).adapter = mockAdapter;
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    (api.defaults as { adapter?: unknown }).adapter = undefined;
  });

  it('401 → 单飞 refresh → 重放原请求成功', async () => {
    saveRefreshToken('refresh-1');
    vi.stubGlobal('fetch', fetchStub);

    const [a, b] = await Promise.all([api.get('/v1/secure'), api.get('/v1/secure')]);
    expect(a.data.ok).toBe(true);
    expect(b.data.ok).toBe(true);
    expect(refreshCalls).toBe(1); // 单飞：并发 401 仅刷新一次
    expect(accessSeen).toBe('Bearer new-access'); // 重放携带新 access
    expect(store.get('codeaudit.refresh_token')).toBe('refresh-2'); // 滚动续签
  });

  it('refresh 失败 → 清会话（路由守卫接管跳登录）', async () => {
    saveRefreshToken('expired');
    vi.stubGlobal('fetch', fetchStub);
    const assign = vi.fn();
    (globalThis as { window?: unknown }).window = { location: { assign } };
    try {
      const err = await api.get('/v1/secure').then(
        () => null,
        (e: unknown) => e,
      );
      expect(err).toBeTruthy();
      expect(store.get('codeaudit.refresh_token')).toBeUndefined(); // Map.get：缺失即 undefined（clearSession 已执行）
      expect(assign).toHaveBeenCalledWith('/login');
    } finally {
      (globalThis as { window?: unknown }).window = undefined;
    }
  });
});
