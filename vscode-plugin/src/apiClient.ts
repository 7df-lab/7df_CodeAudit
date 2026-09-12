// 平台 REST 客户端：JWT 登录/单飞刷新 + JSON 风格查询参数 + 分页累积。
// 移植自 web/console/src/api/client.ts 的口径（ADR-155 JSON 风格分页参数 / 401 单飞刷新），
// fetch 可注入以便单测（Node 22 全局 fetch / FormData / Blob 均可用）。
import type {
  FindingsPage,
  GitAnchor,
  LoginResponse,
  PaginationResponse,
  Project,
  ScanTask,
  TaskSnapshot,
  TaskSummary,
  ToolInfo,
  UnifiedFinding,
  UploadResult,
} from './types';

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

// ADR-155 口径：网关 decodeQuery 只认 JSON 风格查询参数——标量照常，对象/数组值 JSON 编码。
export function encodeQuery(params: Record<string, unknown>): string {
  const sp = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;
    sp.append(key, typeof value === 'object' ? JSON.stringify(value) : String(value));
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body?: unknown,
  ) {
    super(message);
  }
}

export interface TokenStore {
  getAccessToken(): string;
  setTokens(access: string, refresh?: string): void;
  getRefreshToken(): string;
  /** silent=true：主动登出——不触发上层"会话失效"告警（用户已知晓） */
  clear(silent?: boolean): void;
}

// 429 限流退避（对齐 console client.ts noteRateLimit：5~60s 钳位）
export function backoffMs(retryAfterS: number | undefined, nowMs: number): number {
  const s = Math.min(Math.max(retryAfterS ?? 15, 5), 60);
  return nowMs + s * 1000;
}

// REST 超时（B2-7）：模块级常量 + export（可测试、可被调用方引用）。
// 经 AbortSignal.timeout 注入 fetch——网关挂死时请求不再无限悬挂（轮询/watcher
// 会堆积在途请求）。Node ≥17.3 / 现代浏览器原生支持。
export const REQUEST_TIMEOUT_MS = 60_000; // 普通 JSON 请求
export const UPLOAD_TIMEOUT_MS = 600_000; // 工作区 zip 上传（可达数十 MB）走 10 分钟长档

export class CodeAuditClient {
  private refreshInFlight: Promise<string> | null = null;
  public rateLimitUntil = 0;
  /**
   * 网关连通性（最近一次请求视角）：收到任何 HTTP 响应（含 4xx/5xx）= 可达；
   * fetch 网络层抛错（ECONNREFUSED/DNS/超时）= 不可达。与 isLoggedIn 正交——
   * isLoggedIn 只表示"本地存有凭据"，后端宕机时凭据仍在，UI 必须能区分两者。
   */
  public offline = false;
  private readonly onStateChange?: () => void;

  constructor(
    public baseUrl: string,
    private tokens: TokenStore,
    private fetchFn: FetchLike = (u, i) => fetch(u, i),
    opts: { onStateChange?: () => void } = {},
  ) {
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.onStateChange = opts.onStateChange;
  }

  /** 连通性翻转时才通知（避免每请求刷 UI） */
  private markOffline(offline: boolean): void {
    if (this.offline === offline) return;
    this.offline = offline;
    this.onStateChange?.();
  }

  async login(username: string, password: string): Promise<LoginResponse> {
    const data = await this.requestJson<LoginResponse>('POST', '/v1/auth/login', { username, password }, { skipAuth: true });
    this.tokens.setTokens(data.access_token, data.refresh_token);
    return data;
  }

  async logout(): Promise<void> {
    try {
      await this.requestJson('POST', '/v1/auth/logout', { access_token: this.tokens.getAccessToken() }, { skipAuth: true });
    } finally {
      this.tokens.clear(true); // 用户主动登出：静默清凭据，不触发"会话失效"告警（B5-2）
    }
  }

  isLoggedIn(): boolean {
    return !!this.tokens.getAccessToken() || !!this.tokens.getRefreshToken();
  }

  // 列表自动翻页累积（listProjects/listTasks/listFindings 共用）：page_size=100，
  // 上限 50 页防御——网关缺省 page_size=20/上限 100（proto L244），不带分页裸调
  // 只拿首页，条目多时静默截断。
  private async paginateAll<P extends { pagination?: PaginationResponse }, R>(
    path: string,
    extra: Record<string, unknown>,
    pick: (page: P) => R[],
  ): Promise<R[]> {
    const all: R[] = [];
    let cursor = '';
    for (let i = 0; i < 50; i++) {
      const page = await this.requestJson<P>('GET', path, undefined, {
        query: { ...extra, pagination: { page_size: 100, cursor } },
      });
      all.push(...pick(page));
      if (!page.pagination?.has_next) break;
      cursor = page.pagination.next_cursor;
    }
    return all;
  }

  async listProjects(): Promise<Project[]> {
    return this.paginateAll<{ projects?: Project[]; pagination?: PaginationResponse }, Project>('/v1/projects', {}, (p) => p.projects ?? []);
  }

  /** 任务列表（按创建时间倒序）；projectId 缺省时返回全部项目任务 */
  async listTasks(projectId?: string): Promise<TaskSummary[]> {
    return this.paginateAll<{ tasks?: TaskSummary[]; pagination?: PaginationResponse }, TaskSummary>(
      '/v1/tasks',
      projectId ? { project_id: projectId } : {},
      (p) => p.tasks ?? [],
    );
  }

  async listTools(): Promise<ToolInfo[]> {
    const data = await this.requestJson<{ tools: ToolInfo[] }>('GET', '/v1/tools');
    return data.tools ?? [];
  }

  async uploadArchive(zip: Blob, filename = 'workspace.zip'): Promise<UploadResult> {
    const fd = new FormData();
    fd.append('file', zip, filename);
    return this.requestJson<UploadResult>('POST', '/v1/uploads/archive', fd);
  }

  /** 增量扫描载荷（ADR-225 D5 类型化契约；全量任务不携带任何键，行为与旧客户端一致） */
  async createTask(
    projectId: string,
    scanMode: string,
    sastTools: string[],
    config: Record<string, string>,
    incremental?: { baseline_task_id?: string; git_anchor?: GitAnchor | null; diff_hint?: string },
  ): Promise<ScanTask> {
    const body: Record<string, unknown> = {
      project_id: projectId,
      scan_mode: scanMode,
      sast_tools: sastTools,
      config,
    };
    if (incremental) {
      // incremental=true + 锚点/提示；显式基线仅在用户明确指定时携带（当前 UX 为自动选定）
      body.incremental = true;
      if (incremental.baseline_task_id) body.baseline_task_id = incremental.baseline_task_id;
      if (incremental.git_anchor) body.git_anchor = incremental.git_anchor;
      if (incremental.diff_hint) body.diff_hint = incremental.diff_hint;
    }
    return this.requestJson<ScanTask>('POST', '/v1/tasks', body);
  }

  async startTask(taskId: string): Promise<void> {
    await this.requestJson('POST', `/v1/tasks/${taskId}/start`, {});
  }

  /** 取消运行中的任务（网关 POST /v1/tasks/{id}/cancel → task-service CancelScanTask） */
  async cancelTask(taskId: string): Promise<void> {
    await this.requestJson('POST', `/v1/tasks/${taskId}/cancel`, {});
  }

  /** 暂停运行中的任务（网关 POST /v1/tasks/{id}/pause → TASK_STATUS_PAUSED，非终态可恢复） */
  async pauseTask(taskId: string): Promise<void> {
    await this.requestJson('POST', `/v1/tasks/${taskId}/pause`, {});
  }

  /** 恢复暂停中的任务（网关 POST /v1/tasks/{id}/resume → TASK_STATUS_RUNNING） */
  async resumeTask(taskId: string): Promise<void> {
    await this.requestJson('POST', `/v1/tasks/${taskId}/resume`, {});
  }

  /**
   * 任务快照聚合口（task/progress/logs/ai 四路一次响应）。cursors 为增量游标：
   * logs_after=已见最后 log_id（只回严格更大的条目）、ai_cursor=AI 正文字节偏移
   * （只回更大偏移的 chunk）——轮询回退与 WS 重连续订共用同一口径。
   */
  async taskSnapshot(
    taskId: string,
    cursors?: { logsAfter?: string; aiCursor?: number | string },
  ): Promise<TaskSnapshot> {
    return this.requestJson<TaskSnapshot>('GET', `/v1/tasks/${taskId}/snapshot`, undefined, {
      query: cursors?.logsAfter !== undefined || cursors?.aiCursor !== undefined
        ? { logs_after: cursors?.logsAfter, ai_cursor: cursors?.aiCursor }
        : undefined,
    });
  }

  // 单任务 findings：自动翻页累积（与 console FindingsPage 口径一致，走 paginateAll）
  async listFindings(taskId: string): Promise<UnifiedFinding[]> {
    return this.paginateAll<FindingsPage, UnifiedFinding>('/v1/findings', { task_id: taskId }, (p) => p.findings ?? []);
  }

  // 刷新直接裸 fetch：不经 requestJson 的 401 拦截（避免刷新请求自身 401 触发递归刷新）
  private async doRefresh(): Promise<string> {
    const refresh = this.tokens.getRefreshToken();
    if (!refresh) {
      this.tokens.clear(); // 会话不可恢复：清空，交由调用方引导重新登录
      throw new ApiError(401, 'no refresh token');
    }
    let resp: Response;
    try {
      resp = await this.fetchFn(`${this.baseUrl}/v1/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refresh }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
    } catch (e) {
      this.markOffline(true); // 网络层失败 ≠ 会话失效：不清凭据，仅标离线
      throw e;
    }
    this.markOffline(false);
    if (!resp.ok) {
      // 仅 401 = 服务端明确拒绝该 refresh token（会话失效）才清凭据（B2-3）：
      // 502/429 等瞬态失败保留缓存 token——网关抖动/限流不该把用户登出，
      // 离线态下凭据仍在（isLoggedIn 保持 true，恢复后可自动续期）
      if (resp.status === 401) this.tokens.clear();
      throw new ApiError(resp.status, `refresh failed: ${resp.status}`);
    }
    const data = (await resp.json()) as LoginResponse;
    this.tokens.setTokens(data.access_token, data.refresh_token);
    return data.access_token;
  }

  private async requestJson<T>(
    method: string,
    path: string,
    body?: unknown,
    opts: { skipAuth?: boolean; query?: Record<string, unknown> } = {},
  ): Promise<T> {
    const call = async (token: string, retried: boolean): Promise<T> => {
      const url = `${this.baseUrl}${path}${opts.query ? encodeQuery(opts.query) : ''}`;
      const headers: Record<string, string> = {};
      if (token) headers.Authorization = `Bearer ${token}`;
      if (body !== undefined && !(body instanceof FormData)) headers['Content-Type'] = 'application/json';
      let resp: Response;
      try {
        resp = await this.fetchFn(url, {
          method,
          headers,
          body: body instanceof FormData ? body : body === undefined ? undefined : JSON.stringify(body),
          // 超时档位按请求形态区分（B2-7）：multipart 上传走 600s 长档，其余 60s
          signal: AbortSignal.timeout(body instanceof FormData ? UPLOAD_TIMEOUT_MS : REQUEST_TIMEOUT_MS),
        });
      } catch (e) {
        // 网络层失败（ECONNREFUSED/DNS/代理拒绝）：网关不可达。不清凭据（离线 ≠ 会话失效），
        // 仅翻转离线态供 UI 降级展示；后端宕机时isLoggedIn仍为 true 属预期语义
        this.markOffline(true);
        throw e;
      }
      this.markOffline(false); // 收到任何 HTTP 响应（含 4xx/5xx）= 网关可达
      if (resp.status === 429) {
        // body 只读一次（回归锁）：先 json() 再 text() 的双读会因 body 已消费把响应体
        // 静默吞成空串——text 读取后就地解析 retry_after 并直接抛出（body 全文随错误透出）
        const text = await resp.text().catch(() => '');
        let retryAfter: number | undefined;
        try {
          retryAfter = Number((JSON.parse(text) as { retry_after?: number }).retry_after);
        } catch {
          /* body 非 JSON：按缺省退避 */
        }
        this.rateLimitUntil = backoffMs(Number.isFinite(retryAfter) ? retryAfter : undefined, Date.now());
        throw new ApiError(429, `${method} ${path} -> 429: ${text.slice(0, 300)}`, text);
      }
      if (resp.status === 401 && !retried && !opts.skipAuth) {
        const fresh = await this.singleFlightRefresh();
        return call(fresh, true);
      }
      if (!resp.ok) {
        const text = await resp.text().catch(() => '');
        throw new ApiError(resp.status, `${method} ${path} -> ${resp.status}: ${text.slice(0, 300)}`, text);
      }
      return (await resp.json()) as T;
    };
    return call(opts.skipAuth ? '' : this.tokens.getAccessToken(), false);
  }

  // 单飞刷新：并发 401 共享同一次刷新
  private singleFlightRefresh(): Promise<string> {
    this.refreshInFlight ??= this.doRefresh().finally(() => {
      this.refreshInFlight = null;
    });
    return this.refreshInFlight;
  }
}
