// axios 实例 + 401 单飞刷新（14号 §2.1：内存 access + localStorage refresh，Q3 裁决；
// 刷新单飞防止并发请求引发刷新风暴）
import axios, { type AxiosError, type InternalAxiosRequestConfig } from 'axios';
import { API_ERROR_EVENT, API_OK_EVENT } from './apiEvents';
import type {
  AgentProviderInfo, CreateInviteCodeBody, ReportRow, InferenceProvider, InferenceRoute, InviteCode, InviteCodesListResp,
  ListProjectsResponse, OrchestrationInfo, Project, RegistrationPolicy, ScanTask,
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
      } catch (refreshErr) {
        refreshInFlight = null;
        // B5-P2-4: 刷新失败分类——仅凭据确死才清会话跳登录；5xx/429/网络异常保会话
        // 仅 reject（下轮请求自然重试刷新）。
        if (refreshAuthDead(refreshErr)) {
          clearSession();
          if (typeof window !== 'undefined') window.location.assign('/login');
        }
        return Promise.reject(error);
      }
    }
    if (status === 429) {
      const retryAfter = Number((error.response?.data as { retry_after?: number } | undefined)?.retry_after);
      noteRateLimit(Number.isFinite(retryAfter) ? retryAfter : undefined);
    }
    // 503 自动重试 3 次退避（1s/2s/4s）；耗尽后 reject + 横幅。
    // B5-P2-5: 仅幂等 GET 自动重放——POST 盲重放且网关每次现生成新幂等键，重复建任务/
    // 重复写风险（100MB 上传最坏盲重放 300MB）；非 GET 直接走下方降级横幅。
    if (status === 503 && (cfg?.method ?? 'get').toLowerCase() === 'get' && cfg
        && (cfg._retry503 ?? 0) < 3) {
      cfg._retry503 = (cfg._retry503 ?? 0) + 1;
      await new Promise((r) => setTimeout(r, 1000 * 2 ** ((cfg._retry503 ?? 1) - 1)));
      return api.request(cfg);
    }
    const isAuth = cfg?.url?.includes('/v1/auth/');
    // ADR-257: 403 不再进全局错误层（旧形态=全屏遮罩毁导航壳，非 admin 死局根因之一）——
    // 页面级 403 由 RequireAdmin/页内空态承接，操作级 403 由各 mutation onError toast。
    if ((status === 501 || status === 503) && cfg && !isAuth
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
  if (!resp.ok) {
    const err = new Error(`refresh failed: ${resp.status}`) as Error & { status?: number };
    err.status = resp.status; // B5-P2-4: 带状态供拦截器分类（凭据死 vs 后端抖动）
    throw err;
  }
  const data = (await resp.json()) as LoginResponse;
  accessToken = data.access_token;
  if (data.refresh_token) saveRefreshToken(data.refresh_token);
  return data.access_token;
}

// B5-P2-4: 刷新失败分类谓词——只有凭据确死（400/401/403/无令牌）才应终止会话。
// 5xx/429/网络异常 = 后端抖动：保住仍有效的 7 天 refresh_token 等下轮自愈。
// access 30min 过期后周期性刷新必经此路，恰逢抖动即踢线是过度误杀（P-22 同族第二通道；
// 网关 /v1/auth/* 按 IP 限流，共享出口 IP 团队可全员中招）。
function refreshAuthDead(err: unknown): boolean {
  const st = (err as { status?: number }).status;
  if (st === 400 || st === 401 || st === 403) return true;
  return err instanceof Error && err.message.includes('no refresh token');
}

// axios 错误 → HTTP 状态码（审计 B3-3：mutation onError 文案统一携带状态码；
// 403/503 等另有全局事件总线横幅，此处只补页面级即时反馈）
export function errStatus(e: unknown): number | undefined {
  return (e as { response?: { status?: number } } | undefined)?.response?.status;
}

// axios 错误 → 网关 {error} 原文优先的可读信息（交互审计 Top4-④ 推广：
// 非法状态转换/策略拒绝等服务端具体原因曾被 axios 通用文案
// "Request failed with status code 409" 吞掉；与 errStatus 组合成
// "失败（HTTP x）：详情" 统一口径）
export function apiErrorMessage(e: unknown): string {
  const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
  return detail || (e as Error)?.message || '未知错误';
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

// ADR-256: 服务端排序/筛选透传（filter=白名单 name CONTAINS / default_scan_mode EQ；
// sort 键白名单 name|created_at）。
export interface ServerListParams {
  pagination?: { page_size: number; cursor?: string };
  filter?: { field: string; operator: string; value: string };
  sort?: { field: string; order: 'SORT_ORDER_ASC' | 'SORT_ORDER_DESC' };
}

export async function getProjects(p?: ServerListParams): Promise<ListProjectsResponse> {
  const params: Record<string, unknown> = {};
  if (p?.pagination) params.pagination = { ...p.pagination, cursor: p.pagination.cursor ?? '' }; // E-12 契约：空游标显式保留
  if (p?.filter) params.filter = { conditions: [p.filter] };
  if (p?.sort) params.sort = p.sort;
  return (await api.get('/v1/projects', Object.keys(params).length ? { params } : undefined)).data;
}

// B5-P2-7: 下拉/索引用途的全量项目——循环翻页直到 has_next=false。
// 服务端 project handler 恒发精确 pagination（has_next 精确 + 数字偏移游标），循环可靠
// 终止；上限 10 页（=1000 项目）防异常 has_next 恒真打爆。此前下拉调 getProjects() 默认页
// 只拿最新 20 条（服务端默认 pageSize=20），项目 >20 后旧项目在下拉/筛选/深链预选不可达。
export async function listAllProjects(): Promise<Project[]> {
  const out: Project[] = [];
  let cursor = '';
  for (let i = 0; i < 10; i++) {
    const resp = await getProjects({ pagination: { page_size: 100, cursor } });
    out.push(...resp.projects);
    if (!resp.pagination?.has_next || !resp.pagination.next_cursor) break;
    cursor = resp.pagination.next_cursor;
  }
  return out;
}

// 任务域全量（2026-10-08 统计冒充总数修复）：服务端按 proto 契约钳 page_size 上限
// 100（"默认20，最大100"），此前前端一次请求 200 被静默截到 100、再拿页长冒充总数
// （107 实录：总数 569 显示 100、状态分布只描述最近 100 条）。走 offset 游标翻全量
// （同 listAllProjects 口径；排序键=created_at 不可变，R68，页间稳定）。护栏 30 页
// （=3000 条）：超限计数口径退化为"最近 3000 条"——护栏是防异常 has_next 恒真打爆，
// 不是常态路径。task_service 仅在有下页时才填 Pagination（B5-P1-2 同源），末页
// 无 pagination 键=循环天然终止。
export const MAX_TASK_WALK_PAGES = 30;
export async function listAllTasks(): Promise<ScanTask[]> {
  const out: ScanTask[] = [];
  let cursor = '0';
  for (let i = 0; i < MAX_TASK_WALK_PAGES; i++) {
    const resp = (await api.get('/v1/tasks', {
      params: { pagination: { page_size: 100, cursor } },
    })).data as { tasks: ScanTask[]; pagination?: { has_next?: boolean; next_cursor?: string } };
    out.push(...resp.tasks);
    if (!resp.pagination?.has_next || !resp.pagination.next_cursor) break;
    cursor = resp.pagination.next_cursor;
  }
  return out;
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
// 注意：服务端 config 存储为整 map 替换（Upsert 整记录，无键级合并）——"只改一键"必须
// GET 现状后带全量键回写，漏带的键会被清掉。
export async function updateProjectConfig(projectId: string, config: Record<string, string>): Promise<ProjectConfigResponse> {
  return (await api.put(`/v1/projects/${projectId}/config`, { config: { project_id: projectId, config } })).data;
}

// proto L872/L1380: UpdateProject → PUT /v1/projects/:id，body=UpdateProjectRequest{project}。
// 服务端整体替换（project-service Update 无字段合并）——调用方必须 GET 全量读回后改字段
// 整对象提交，漏带字段会被清零；gateway 钉死 body.project.project_id=URL 路径段（R73/R91
// 错位防线），repo_url 过服务端安全白名单（危险 scheme 400）。
export async function updateProject(projectId: string, project: Project): Promise<Project> {
  return (await api.put(`/v1/projects/${projectId}`, { project: { ...project, project_id: projectId } })).data;
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
  agent?: string; // 切片 7：任务级 Agent（空/默认=不发——部署默认 dsh）
}

export async function createTask(payload: CreateTaskPayload): Promise<{ task_id: string }> {
  return (await api.post('/v1/tasks', payload)).data;
}

// ===== 编排（负载 pack）生命周期（编排 CRUD 批 2026-09-21）=====
// append-only 修订语义：update=append 新修订；deprecate=停用标记（可恢复）。
// spec 以全量 YAML 传输（服务端 Validate 校验，错误带字段路径回显）。

export async function listOrchestrations(params: { include_deprecated?: boolean; include_history?: boolean }): Promise<{ orchestrations: OrchestrationInfo[] }> {
  return (await api.get('/v1/orchestrations', { params })).data;
}

export async function createOrchestration(name: string, specYAML: string): Promise<OrchestrationInfo> {
  return (await api.post('/v1/orchestrations', { name, spec_yaml: specYAML })).data;
}

export async function updateOrchestration(name: string, specYAML: string): Promise<OrchestrationInfo> {
  return (await api.put(`/v1/orchestrations/${encodeURIComponent(name)}`, { name, spec_yaml: specYAML })).data;
}

export async function deprecateOrchestration(name: string, undePrecate = false): Promise<OrchestrationInfo> {
  return (
    await api.delete(`/v1/orchestrations/${encodeURIComponent(name)}`, {
      // unde_precate 拼写来自 proto 字段 UndePrecate（历史固化，改名=契约破坏）；
      // 网关 query 解析与 engine DeprecateOrchestration.UndePrecate 同名消费。
      params: undePrecate ? { unde_precate: true } : {},
    })
  ).data;
}

// ===== 图节点人机共编辑（v0.5，graph-engine-v0.5-blackboard-hoc.md §4/§2.1）=====
// 追加消息=回合边界生效（bridge 排队，不打断当回合）；claim=human 占住节点
// （调度跳过，TTL 租约）；release=释放回 open。（端点未登记 external-interfaces E-锚——
// 图批次遗留补档缺口，原注释误称 E-57..E-60 实为他端点锚，2026-10-04 撞号清偿时纠正）

export async function injectGraphNodeMessage(taskId: string, nodeId: string, message: string): Promise<{ accepted: boolean; node_status: string; event_seq?: number }> {
  return (await api.post(`/v1/tasks/${taskId}/graph/nodes/${encodeURIComponent(nodeId)}/message`, { message })).data;
}

export async function claimGraphNode(taskId: string, nodeId: string): Promise<{ accepted: boolean; holder: string; lease_until: string }> {
  return (await api.post(`/v1/tasks/${taskId}/graph/nodes/${encodeURIComponent(nodeId)}/claim`, {})).data;
}

export async function releaseGraphNode(taskId: string, nodeId: string): Promise<{ accepted: boolean; node_status: string }> {
  return (await api.delete(`/v1/tasks/${taskId}/graph/nodes/${encodeURIComponent(nodeId)}/claim`)).data;
}

export async function reopenGraphNode(taskId: string, nodeId: string, reason: string): Promise<{ accepted: boolean; node_status: string }> {
  return (await api.post(`/v1/tasks/${taskId}/graph/nodes/${encodeURIComponent(nodeId)}/reopen`, { reason })).data;
}

// 启动续签（ADR-145 补全）：页面刷新后用 refresh_token 静默恢复 access（14号 §2.1 Q3 口径）。
// B5-P2-4 同口径：仅凭据确死才清 refresh_token（boot 落登录页）；抖动时保留令牌，
// 用户重新登录成功自然滚动换新。
export async function bootRefresh(): Promise<string | null> {
  if (!readRefreshToken()) return null;
  try {
    return await requestRefresh();
  } catch (e) {
    if (refreshAuthDead(e)) clearSession();
    return null;
  }
}

// ===== 推理 provider/路由管理（ADR-217；engine gateway /v1/inference/*，admin 面）=====
// 凭据只进不出：credentials 仅出现在 upsert/update 请求体；读端点类型无该字段。

// wsQ — workspace 查询参数后缀（空串=全局默认 default；切片3b team 上车）。
// engine 转码对六个推理端点统一认 ?workspace=（PUT/POST 上 query 覆盖 body 同名
// 字段——路径名权威同型），故统一走 query 最简。
const wsQ = (ws?: string) => (ws ? `?workspace=${encodeURIComponent(ws)}` : '');

// workspace 清单（切片3b：Provider 页工作区选择器数据源；admin 面）
export interface WorkspaceInfo {
  name: string;
  phase: string;
  labels: Record<string, string>;
}
export async function getWorkspaces(): Promise<{ workspaces: WorkspaceInfo[] }> {
  return (await api.get('/v1/workspaces')).data;
}

// 建区/删区（admin；409 重名/资源守卫的 {error} 原文比 axios 文案可行动——透出）
export async function createWorkspace(payload: { name: string }): Promise<WorkspaceInfo> {
  try {
    return (await api.post('/v1/workspaces', payload)).data;
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
  }
}

export async function deleteWorkspace(name: string): Promise<{ name: string; deleted: boolean }> {
  try {
    return (await api.delete(`/v1/workspaces/${encodeURIComponent(name)}`)).data;
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
  }
}

export async function getInferenceProviders(ws?: string): Promise<{ providers: InferenceProvider[] }> {
  return (await api.get('/v1/inference/providers' + wsQ(ws))).data;
}

export interface InferenceProviderPayload {
  type: string;
  credentials: Record<string, string>;
  config: Record<string, string>;
}

// POST（upsert 语义，created 标识走 Create 还是 Update——manager 判存在性）。
// 错误详情透出：engine R55 键约定守卫/manager INVALID_ARGUMENT 400（类型不支持/
// 类型不可改）的 {error} 原文比 axios 通用文案可读——同 setInferenceRoute 口径
//（2026-09-17 UI 实测：R55 拒别名键曾被吞成 "Request failed with status code 400"）。
async function inferenceProviderCall(
  run: () => Promise<{ data: UpsertInferenceProviderResponse }>,
): Promise<UpsertInferenceProviderResponse> {
  try {
    return (await run()).data;
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
  }
}

export async function createInferenceProvider(
  name: string, payload: InferenceProviderPayload, ws?: string,
): Promise<UpsertInferenceProviderResponse> {
  return inferenceProviderCall(() => api.post('/v1/inference/providers' + wsQ(ws), { name, ...payload }));
}

// PUT /providers/{name}：路径名权威（engine transcode 用路径覆盖 body 同名字段）
export async function updateInferenceProvider(
  name: string, payload: InferenceProviderPayload, ws?: string,
): Promise<UpsertInferenceProviderResponse> {
  return inferenceProviderCall(() => api.put(`/v1/inference/providers/${encodeURIComponent(name)}` + wsQ(ws), payload));
}

export async function deleteInferenceProvider(name: string, ws?: string): Promise<{ deleted: boolean }> {
  return (await api.delete(`/v1/inference/providers/${encodeURIComponent(name)}` + wsQ(ws))).data;
}

export async function getInferenceRoute(ws?: string): Promise<InferenceRoute> {
  return (await api.get('/v1/inference/route' + wsQ(ws))).data;
}

// 切路由：no_verify 默认 false = 网关连通性验证，回执带 validated_endpoints。
// 验证失败的服务端 {error} 详情比 axios 通用文案可读（连通性诊断直出），同 getSourceFile 口径。
export async function setInferenceRoute(
  payload: { provider: string; model: string; no_verify?: boolean }, ws?: string,
): Promise<SetInferenceRouteResponse> {
  try {
    return (await api.put('/v1/inference/route' + wsQ(ws), payload)).data;
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error;
    throw new Error(detail || (e as Error).message);
  }
}

// 报告内容拉取（ADR-150）：下载端点返回报告正文（JSON 或 HTML 文本），
// 供任务详情页内联渲染"报告初步判断"，免去 报告中心→在线查看 的绕行。
export async function getReportContent(reportId: string): Promise<{ format: string; content: string }> {
  const resp = await api.get(`/v1/reports/${reportId}/download`, { responseType: 'text', transformResponse: [(d) => d] });
  // 真实 axios 的 responseType:'text' 恒产字符串；Blob 兜底：mock 台/未来网关变体可能
  // 直给 Blob 载荷（jsdom 之外 .text() 可用）——[object Blob] 冒充正文是静默错数据
  const data = resp.data as string | Blob;
  const content = typeof data === 'string' ? data : await data.text();
  const text = String(content ?? '');
  const format = text.trimStart().startsWith('<') ? 'html' : 'json';
  return { format, content: text };
}

// 报告窗口 CSP meta（审计 B3-2，纵深防御·双保险第一层）：报告 HTML 仅内联样式、无脚本——
// 写入报告窗口前先注入此 meta（必须先于任何内容写入），default-src 'none' 全灭脚本/
// 外链资源，style-src 'unsafe-inline' 保内联样式活。
export const REPORT_WINDOW_CSP_META =
  '<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'">';

// B5(P3-a)：meta 插入位置 doctype 感知——后端报告首字节是 <!doctype html>，旧实现把
// meta 拼在最前会把 doctype 挤到非首位 → 文档落 quirks 模式（表格/行高随样式演进漂移）。
// 有 doctype 时插到其后（CSP meta 在 head 前同样生效），无 doctype 维持首字节前置。
export function withReportCspMeta(content: string): string {
  // doctype 前允许可有空白/BOM/HTML 注释（复审 R：模板演进可能出现注释前导变体）
  const m = /^(?:\s|<!--[\s\S]*?-->)*<!doctype\s+html[^>]*>/i.exec(content);
  return m
    ? content.slice(0, m[0].length) + REPORT_WINDOW_CSP_META + content.slice(m[0].length)
    : REPORT_WINDOW_CSP_META + content;
}

// 报告窗口渲染（fix-plan-0911 §14 升级，双保险第二层·更硬）：打开 about:blank 宿主窗，
// 在其中插入 <iframe sandbox src=blob:> 携带报告内容——sandbox 空 token 不含
// allow-scripts / allow-same-origin，iframe 内脚本与同源权限全灭（纯渲染不需要任何
// 能力，比 meta CSP 更硬）；同时 CSP meta 由本函数统一前置于 blob 内容，即便
// sandbox 通道被降级（旧浏览器/扩展改写 iframe）meta 仍灭脚本。
// HTML 分支原样传入；JSON 分支由调用方转义为 <pre> 文本后传入（保持旧转义行为）。
export function openReportWindow(content: string, mime: 'text/html' | 'text/plain'): Window | null {
  const w = window.open('about:blank', '_blank');
  if (!w) return null;
  const blob = new Blob([withReportCspMeta(content)], { type: `${mime};charset=utf-8` });
  const url = URL.createObjectURL(blob);
  const iframe = w.document.createElement('iframe');
  // 无 allow-scripts：脚本全灭；无 allow-same-origin：来源隔离（blob 亦不继承宿主源）
  iframe.setAttribute('sandbox', '');
  iframe.setAttribute('src', url);
  iframe.setAttribute('title', '报告内容');
  iframe.setAttribute('style', 'border:0;width:100%;height:100vh;display:block');
  w.document.body.appendChild(iframe);
  // B5(P3-a)：blob URL 生命周期回收——宿主窗卸载（用户关闭报告窗）即 revoke。此前每次
  // "在线查看"泄漏一份报告正文的 blob 引用直至宿主文档卸载。不在 iframe load 即 revoke：
  // 窗口开着期间 blob 须保持可取（ui_check 等外部校验 fetch(blob:) 与其兼容），关闭即释放。
  // 复审 R：beforeunload 在未交互 about:blank 窗口的触发语义跨浏览器不保证——加 opener 侧
  // w.closed 轮询兜底（1s 粒度，关窗即停），语义确定性优先。
  w.addEventListener('beforeunload', () => URL.revokeObjectURL(url));
  const closedPoll = setInterval(() => {
    if (w.closed) {
      URL.revokeObjectURL(url);
      clearInterval(closedPoll);
    }
  }, 1000);
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

// 链路 chip 文件存在性探测（2026-09-13 误挂接根治）：resolve_only=1 只做根内解析
// 不读内容（engine bfd47b42）。三态语义：
//   'exists'  = 命中（200，含超限/二进制文件——存在性 ≠ 可读性）；
//   'missing' = 仅认 404 且 {error} 含 "file not found in project"（文件级未命中）——
//               源根不可解析（404 同码）或网关故障等一律 'unknown'，fail-open 等价现状，
//               避免把"树被清理"误判为"AI 幻觉引用"；
//   'unknown' = 老网关（忽略未知参数照常回全文 → 200 也是 'exists'，平滑兼容）。
export type SourceFileProbe = 'exists' | 'missing' | 'unknown';
export async function probeSourceFile(taskId: string, path: string): Promise<SourceFileProbe> {
  try {
    await api.get(`/v1/tasks/${taskId}/source-file`, { params: { path, resolve_only: 1 } });
    return 'exists';
  } catch (e) {
    const status = errStatus(e);
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error ?? '';
    if (status === 404 && detail.includes('file not found in project')) return 'missing';
    return 'unknown';
  }
}

// ---- Agent 清单与切换（TP20-T2 切换 Agent 收口；伞仓计划 §3.1）----

// GET /v1/agents：登录可读；创建页与切换 Modal 同源（硬编码清单退役）。
// 端点失败=调用方降级（入口禁用/锁定默认）——不回落硬编码（计划 §0 裁决）。
export async function listAgents(): Promise<{ agents: AgentProviderInfo[] }> {
  return (await api.get('/v1/agents')).data;
}

// POST /v1/tasks/{id}/agent：PAUSED/QUEUED 态切换。RUNNING 态由调用方
// （AgentSwitchModal）直发 pause?agent= 回合边界换手——dispatchAction 的动作面
// 不带 agent 参数（stateMachine ALLOWED_ACTIONS 不动=计划 §3 裁决，切换是带
// 语义的暂停而非通用动作参数化）。
export async function switchTaskAgent(taskId: string, agent: string): Promise<unknown> {
  return (await api.post(`/v1/tasks/${encodeURIComponent(taskId)}/agent`, { agent })).data;
}

// ---- V2.2 邀请码管理（ADR-255）：形状单点锚定于此（ADR-203，页面禁手写形状）----

// GET /v1/invites：管理端列表（?include_terminal= 默认仅有效码；config 注入码只读随行）。
export async function listInviteCodes(opts?: { includeTerminal?: boolean; cursor?: string }): Promise<InviteCodesListResp> {
  const pagination = opts?.cursor ? { cursor: opts.cursor } : undefined;
  return (await api.get('/v1/invites', {
    params: { include_terminal: opts?.includeTerminal || undefined, pagination },
  })).data;
}

// POST /v1/invites：生成（created_by 由网关注入 JWT sub，body 不传）。
export async function createInviteCode(body: CreateInviteCodeBody): Promise<InviteCode> {
  return (await api.post('/v1/invites', body)).data;
}

// POST /v1/invites/:id/revoke：状态翻转（幂等，活跃码唯一下线通道）。
export async function revokeInviteCode(id: string): Promise<InviteCode> {
  return (await api.post(`/v1/invites/${id}/revoke`, {})).data;
}

// DELETE /v1/invites/:id：仅终态码（Q14 两步走，后端护栏）。
export async function deleteInviteCode(id: string): Promise<void> {
  await api.delete(`/v1/invites/${id}`);
}

// GET /v1/auth/registration-policy：公开读（/v1/auth 链免 JWT），仅 mode 三值。
export async function getRegistrationPolicy(): Promise<RegistrationPolicy> {
  return (await api.get('/v1/auth/registration-policy')).data;
}

// GET /v1/reports（ADR-256：offset 游标+total 契约 + 服务端排序/筛选；filter 仅 format EQ）。
export interface ReportsListResp {
  reports: ReportRow[];
  pagination?: { next_cursor?: string; has_next?: boolean; total?: number };
}

export async function listReports(p?: ServerListParams & { task_id?: string }): Promise<ReportsListResp> {
  const params: Record<string, unknown> = {};
  if (p?.task_id) params.task_id = p.task_id;
  if (p?.pagination) params.pagination = { ...p.pagination, cursor: p.pagination.cursor ?? '' };
  if (p?.filter) params.filter = { conditions: [p.filter] };
  if (p?.sort) params.sort = p.sort;
  return (await api.get('/v1/reports', Object.keys(params).length ? { params } : undefined)).data;
}
