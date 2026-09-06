// axios 实例 + 401 单飞刷新（14号 §2.1：内存 access + localStorage refresh，Q3 裁决；
// 刷新单飞防止并发请求引发刷新风暴）
import axios, { type AxiosError, type InternalAxiosRequestConfig } from 'axios';
import { API_ERROR_EVENT, API_OK_EVENT } from './apiEvents';

export const TOKEN_KEY = 'auditmind.refresh_token';

export interface LoginResponse {
  access_token: string;
  refresh_token: string;
  expires_in_s: number;
}

export const api = axios.create({ baseURL: '/' });

// ADR-155: 网关 decodeQuery 只认 JSON 风格查询参数（?pagination={"page_size":20,"cursor":"5"}，
// protojson 直解，TP12-T3 口径）；axios 默认把嵌套对象序列化为 pagination[page_size]=20，
// 网关解不出 cursor → "加载更多"恒返回第一页（GUI 实测 20 行追加后完全重复）。
// 统一序列化：标量照常，对象/数组值 JSON 编码。一处修复全站列表分页生效。
api.defaults.paramsSerializer = {
  serialize(raw: Record<string, unknown>): string {
    const sp = new URLSearchParams();
    for (const [key, value] of Object.entries(raw)) {
      if (value === undefined || value === null) continue;
      sp.append(key, typeof value === 'object' ? JSON.stringify(value) : String(value));
    }
    return sp.toString();
  },
};

let accessToken = '';
export function setAccessToken(token: string): void {
  accessToken = token;
}
export function getAccessToken(): string {
  return accessToken;
}
export function saveRefreshToken(token: string): void {
  globalThis.localStorage?.setItem(TOKEN_KEY, token);
}
export function readRefreshToken(): string {
  return globalThis.localStorage?.getItem(TOKEN_KEY) ?? '';
}
export function clearSession(): void {
  accessToken = '';
  globalThis.localStorage?.removeItem(TOKEN_KEY);
}

api.interceptors.request.use((cfg) => {
  if (accessToken) cfg.headers.Authorization = `Bearer ${accessToken}`;
  return cfg;
});

let refreshInFlight: Promise<string> | null = null;

// 429 限流退避（ADR-170）：被限流时记录退避截止时刻，轮询口读 pollIntervalMs() 拉长间隔，
// 避免错误态下继续按固定频率撞击限流器。
let backoffUntil = 0;
export function noteRateLimit(retryAfterS?: number): void {
  const s = Math.min(Math.max(retryAfterS ?? 15, 5), 60);
  backoffUntil = Date.now() + s * 1000;
}
export function pollIntervalMs(base: number): number {
  const remain = backoffUntil - Date.now();
  return remain > 0 ? Math.ceil(remain) : base;
}

// 单飞刷新：刷新成功重放原请求；失败清会话交由路由守卫跳登录。
// 14号 §3.5 错误映射：401=静默刷新/跳登录（上方）；503=自动重试 3 次退避后降级横幅；
// 403=权限不足页、501=能力未接入 → 派发全局事件由 ApiErrorOverlay 渲染（auth 端点除外）。
api.interceptors.response.use(
  (resp) => {
    if (typeof window !== 'undefined') window.dispatchEvent(new Event(API_OK_EVENT)); // 服务恢复→撤横幅
    return resp;
  },
  async (error: AxiosError) => {
    const status = error.response?.status;
    const cfg = error.config as
      | (InternalAxiosRequestConfig & { _retried?: boolean; _retry503?: number })
      | undefined;
    if (status === 401 && cfg && !cfg._retried && !cfg.url?.includes('/v1/auth/')) {
      cfg._retried = true;
      try {
        refreshInFlight = refreshInFlight ?? requestRefresh();
        const newAccess = await refreshInFlight;
        refreshInFlight = null;
        cfg.headers.Authorization = `Bearer ${newAccess}`;
        return api.request(cfg);
      } catch {
        refreshInFlight = null;
        clearSession();
        if (typeof window !== 'undefined') window.location.assign('/login');
        return Promise.reject(error);
      }
    }
    if (status === 429) {
      const retryAfter = Number((error.response?.data as { retry_after?: number } | undefined)?.retry_after);
      noteRateLimit(Number.isFinite(retryAfter) ? retryAfter : undefined);
    }
    // 503 自动重试 3 次退避（1s/2s/4s）；耗尽后 reject + 横幅
    if (status === 503 && cfg && (cfg._retry503 ?? 0) < 3) {
      cfg._retry503 = (cfg._retry503 ?? 0) + 1;
      await new Promise((r) => setTimeout(r, 1000 * 2 ** ((cfg._retry503 ?? 1) - 1)));
      return api.request(cfg);
    }
    const isAuth = cfg?.url?.includes('/v1/auth/');
    if ((status === 403 || status === 501 || status === 503) && cfg && !isAuth
        && typeof window !== 'undefined') {
      window.dispatchEvent(new CustomEvent(API_ERROR_EVENT, { detail: status }));
    }
    return Promise.reject(error);
  },
);

// 刷新直接走 fetch：不经 api 实例的拦截器（避免刷新请求自身 401 触发递归刷新）
async function requestRefresh(): Promise<string> {
  const refresh = readRefreshToken();
  if (!refresh) throw new Error('no refresh token');
  const resp = await fetch('/v1/auth/refresh', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refresh }),
  });
  if (!resp.ok) throw new Error(`refresh failed: ${resp.status}`);
  const data = (await resp.json()) as LoginResponse;
  accessToken = data.access_token;
  if (data.refresh_token) saveRefreshToken(data.refresh_token);
  return data.access_token;
}

// 代码压缩包上传（ADR-145）：multipart 直传网关，返回解包目录（可作 project_path）。
export async function uploadArchive(file: File): Promise<{ upload_id: string; dir: string; files: number }> {
  const fd = new FormData();
  fd.append('file', file);
  const resp = await api.post('/v1/uploads/archive', fd, {
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: 120_000,
  });
  return resp.data;
}

// 启动续签（ADR-145 补全）：页面刷新后用 refresh_token 静默恢复 access（14号 §2.1 Q3 口径）。
export async function bootRefresh(): Promise<string | null> {
  if (!readRefreshToken()) return null;
  try {
    return await requestRefresh();
  } catch {
    clearSession();
    return null;
  }
}

// 报告内容拉取（ADR-150）：下载端点返回报告正文（JSON 或 HTML 文本），
// 供任务详情页内联渲染"报告初步判断"，免去 报告中心→在线查看 的绕行。
export async function getReportContent(reportId: string): Promise<{ format: string; content: string }> {
  const resp = await api.get(`/v1/reports/${reportId}/download`, { responseType: 'text', transformResponse: [(d) => d] });
  const content = String(resp.data ?? '');
  const format = content.trimStart().startsWith('<') ? 'html' : 'json';
  return { format, content };
}
