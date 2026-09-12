// axios 实例 + 401 单飞刷新（14号 §2.1：内存 access + localStorage refresh，Q3 裁决；
// 刷新单飞防止并发请求引发刷新风暴）
import axios, { type AxiosError, type InternalAxiosRequestConfig } from 'axios';
import { API_ERROR_EVENT, API_OK_EVENT } from './apiEvents';
import type {
  InferenceProvider, InferenceRoute, ListProjectsResponse, Project,
  SetInferenceRouteResponse, ToolInfo, UpsertInferenceProviderResponse,
} from './types';

export const TOKEN_KEY = 'codeaudit.refresh_token';

export interface LoginResponse {
  access_token: string;
  refresh_token: string;
  // B8-web（跨仓契约收口）：expires_in_s 是 proto int64（google.protobuf.Duration 风格秒数），
  // protojson 序列化 int64 可能为字符串形态——类型放宽 string | number，消费点必须 Number() 归一
  // （当前无消费点——login/register/refresh 只取令牌对；将来排期刷新定时器时在此口径下消费）。
  expires_in_s: string | number;
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

// axios 错误 → HTTP 状态码（审计 B3-3：mutation onError 文案统一携带状态码；
// 403/503 等另有全局事件总线横幅，此处只补页面级即时反馈）
export function errStatus(e: unknown): number | undefined {
  return (e as { response?: { status?: number } } | undefined)?.response?.status;
}

// 代码压缩包上限 100MB（2026-09-08 用户指令）：与 gateway uploadMaxBytes（100MB）/
// nginx client_max_body_size 100m 同源；页面 beforeUpload 用作本地预检，超限不发起请求。
export const MAX_ARCHIVE_UPLOAD_BYTES = 100 * 1024 * 1024;

// 代码压缩包上传（ADR-200）：multipart 直传网关 → 网关流式转发 storage（MinIO），
// 网关不落盘。返回 file_id —— 创建任务时放入 config.upload_file_id，
// 启动时 task-service 从 storage 拉回解包扫描。
export interface UploadArchiveResponse {
  upload_id: string;
  file_id: string;
  file_path: string;
  size_bytes: number;
}

export async function uploadArchive(file: File): Promise<UploadArchiveResponse> {
  const fd = new FormData();
  fd.append('file', file);
  const resp = await api.post('/v1/uploads/archive', fd, {
    headers: { 'Content-Type': 'multipart/form-data' },
    // B4-3：120s→300s——大包（接近 100MB 上限）在慢速上行链路（家庭宽带/移动网络）
    // 120s 内传不完，axios 客户端侧超时先于网关 300s 读写窗掐断，用户只见"上传失败"。
    // 与 nginx proxy_read/send_timeout 300s 对齐。
    timeout: 300_000,
  });
  return resp.data;
}

// ===== 端点类型化层（ADR-203）=====
// REST 响应形状在此单点锚定：锚 = proto 响应消息的 protojson 序列化（gateway transcode 直转，
// services/gateway-service/internal/handler/transcode.go）或 gateway 手写 JSON（/v1/tools）。
// 页面禁止 `.data as {手写形状}`——此前 20 处散落 as-cast 是臆造空间（ProjectsPage res.dir 死链路
// 存活三个版本的实证）；形状漂移由 verify.sh G3 的 tsc 红在消费点，而非等 GUI 事故揭发。

export async function getProjects(pagination?: { page_size: number; cursor?: string }): Promise<ListProjectsResponse> {
  const params = pagination
    ? { pagination: { page_size: pagination.page_size, cursor: pagination.cursor ?? '' } }
    : undefined;
  return (await api.get('/v1/projects', params ? { params } : undefined)).data;
}

// proto L845: GetProject 返回裸 Project（protojson 直出，无包装）
export async function getProject(projectId: string): Promise<Project> {
  return (await api.get(`/v1/projects/${projectId}`)).data;
}

export interface ProjectConfigResponse {
  project_id: string;
  config: Record<string, string>;
}

// proto L849/1156: GetProjectConfig → ProjectConfig{project_id, config map<string,string>}
export async function getProjectConfig(projectId: string): Promise<ProjectConfigResponse> {
  return (await api.get(`/v1/projects/${projectId}/config`)).data;
}

// proto L849/L1156/L1158: UpdateProjectConfigRequest{config: ProjectConfig}——双层包装
// （ProjectConfig{project_id, config map<string,string>}）；扁平体会被 protojson 丢字段
// → project-service "project  not found"（E2E 实证后修正）
export async function updateProjectConfig(projectId: string, config: Record<string, string>): Promise<ProjectConfigResponse> {
  return (await api.put(`/v1/projects/${projectId}/config`, { config: { project_id: projectId, config } })).data;
}

// gateway transcode.go tools(): 手写 JSON {tools: [{tool_id,name,supported_languages,output_format,valid,errors}]}
export async function getTools(): Promise<{ tools: ToolInfo[] }> {
  return (await api.get('/v1/tools')).data;
}

export interface CreateProjectPayload {
  name: string;
  repo_url?: string;
  default_branch: string;
  default_scan_mode: string;
}

// proto L844: CreateProject 返回裸 Project
export async function createProject(payload: CreateProjectPayload): Promise<Project> {
  return (await api.post('/v1/projects', { project: payload })).data;
}

export interface CreateTaskPayload {
  project_id: string;
  scan_mode: string;
  sast_tools: string[];
  config: Record<string, string>;
}

export async function createTask(payload: CreateTaskPayload): Promise<{ task_id: string }> {
  return (await api.post('/v1/tasks', payload)).data;
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

// ===== 推理 provider/路由管理（ADR-217；engine gateway /v1/inference/*，admin 面）=====
// 凭据只进不出：credentials 仅出现在 upsert/update 请求体；读端点类型无该字段。

export async function getInferenceProviders(): Promise<{ providers: InferenceProvider[] }> {
  return (await api.get('/v1/inference/providers')).data;
}

export interface InferenceProviderPayload {
  type: string;
  credentials: Record<string, string>;
  config: Record<string, string>;
}

// POST（upsert 语义，created 标识走 Create 还是 Update——manager 判存在性）
export async function createInferenceProvider(
  name: string, payload: InferenceProviderPayload,
): Promise<UpsertInferenceProviderResponse> {
  return (await api.post('/v1/inference/providers', { name, ...payload })).data;
}

// PUT /providers/{name}：路径名权威（engine transcode 用路径覆盖 body 同名字段）
export async function updateInferenceProvider(
  name: string, payload: InferenceProviderPayload,
): Promise<UpsertInferenceProviderResponse> {
  return (await api.put(`/v1/inference/providers/${encodeURIComponent(name)}`, payload)).data;
}

export async function deleteInferenceProvider(name: string): Promise<{ deleted: boolean }> {
  return (await api.delete(`/v1/inference/providers/${encodeURIComponent(name)}`)).data;
}

export async function getInferenceRoute(): Promise<InferenceRoute> {
  return (await api.get('/v1/inference/route')).data;
}

// 切路由：no_verify 缺省 false = 网关连通性验证，回执带 validated_endpoints。
// 验证失败的服务端 {error} 详情比 axios 通用文案可读（连通性诊断直出），同 getSourceFile 口径。
export async function setInferenceRoute(
  payload: { provider: string; model: string; no_verify?: boolean },
): Promise<SetInferenceRouteResponse> {
  try {
    return (await api.put('/v1/inference/route', payload)).data;
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
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

// 报告窗口 CSP meta（审计 B3-2，纵深防御·双保险第一层）：报告 HTML 仅内联样式、无脚本——
// 写入报告窗口前先注入此 meta（必须先于任何内容写入），default-src 'none' 全灭脚本/
// 外链资源，style-src 'unsafe-inline' 保内联样式活。
export const REPORT_WINDOW_CSP_META =
  '<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'">';

// 报告窗口渲染（fix-plan-0911 §14 升级，双保险第二层·更硬）：打开 about:blank 宿主窗，
// 在其中插入 <iframe sandbox src=blob:> 携带报告内容——sandbox 空 token 不含
// allow-scripts / allow-same-origin，iframe 内脚本与同源权限全灭（纯渲染不需要任何
// 能力，比 meta CSP 更硬）；同时 CSP meta 由本函数统一前置于 blob 内容，即便
// sandbox 通道被降级（旧浏览器/扩展改写 iframe）meta 仍灭脚本。
// HTML 分支原样传入；JSON 分支由调用方转义为 <pre> 文本后传入（保持旧转义行为）。
export function openReportWindow(content: string, mime: 'text/html' | 'text/plain'): Window | null {
  const w = window.open('about:blank', '_blank');
  if (!w) return null;
  const blob = new Blob([REPORT_WINDOW_CSP_META + content], { type: `${mime};charset=utf-8` });
  const url = URL.createObjectURL(blob);
  const iframe = w.document.createElement('iframe');
  // 无 allow-scripts：脚本全灭；无 allow-same-origin：来源隔离（blob 亦不继承宿主源）
  iframe.setAttribute('sandbox', '');
  iframe.setAttribute('src', url);
  iframe.setAttribute('title', '报告内容');
  iframe.setAttribute('style', 'border:0;width:100%;height:100vh;display:block');
  w.document.body.appendChild(iframe);
  return w;
}

// 重新生成报告（审计 B3-5 接线，D4 裁定）：POST /v1/tasks/{task_id}/report →
// ReportService/GenerateReport（幂等，网关生成幂等键；旧报告保留，新报告入列后经
// ['reports'] 失效刷新）。报告中心"重新生成"按钮消费。
export async function regenerateReport(taskId: string): Promise<{ result?: { report_id: string } }> {
  return (await api.post(`/v1/tasks/${taskId}/report`, {})).data;
}

// 任务源码全文（ADR-195）：gateway 读任务源树内的文件（项目相对路径或裸文件名，
// 服务端回退解析），供发现详情"代码上下文"全文复核与链路跳转定位。
export interface SourceFileResp {
  path: string;
  content: string;
  total_lines: number;
  bytes: number;
  root_via: string;
  resolved_via: string;
}
export async function getSourceFile(taskId: string, path: string): Promise<SourceFileResp> {
  try {
    const resp = await api.get(`/v1/tasks/${taskId}/source-file`, { params: { path } });
    return resp.data as SourceFileResp;
  } catch (e) {
    // 服务端 writeError 的 {error} 详情比 axios 通用 "status code 404" 更可读（降级横幅展示用）
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
  }
}
