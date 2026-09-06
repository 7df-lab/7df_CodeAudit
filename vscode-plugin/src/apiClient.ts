// 平台 REST 客户端：JWT 登录/单飞刷新 + JSON 风格查询参数 + 分页累积。
// 移植自 web/console/src/api/client.ts 的口径（ADR-155 JSON 风格分页参数 / 401 单飞刷新），
// fetch 可注入以便单测（Node 22 全局 fetch / FormData / Blob 均可用）。
import type {
  FindingsPage,
  LoginResponse,
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
  clear(): void;
}

// 429 限流退避（对齐 console client.ts noteRateLimit：5~60s 钳位）
export function backoffMs(retryAfterS: number | undefined, nowMs: number): number {
  const s = Math.min(Math.max(retryAfterS ?? 15, 5), 60);
  return nowMs + s * 1000;
}

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
      this.tokens.clear();
    }
  }

  isLoggedIn(): boolean {
    return !!this.tokens.getAccessToken() || !!this.tokens.getRefreshToken();
  }

  async listProjects(): Promise<Project[]> {
    const data = await this.requestJson<{ projects: Project[] }>('GET', '/v1/projects');
    return data.projects ?? [];
  }

  /** 任务列表（按创建时间倒序）；projectId 缺省时返回全部项目任务 */
  async listTasks(projectId?: string): Promise<TaskSummary[]> {
    const data = await this.requestJson<{ tasks: TaskSummary[] }>('GET', '/v1/tasks', undefined, {
      query: projectId ? { project_id: projectId } : undefined,
    });
    return data.tasks ?? [];
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

  async createTask(projectId: string, scanMode: string, sastTools: string[], config: Record<string, string>): Promise<ScanTask> {
    return this.requestJson<ScanTask>('POST', '/v1/tasks', {
      project_id: projectId,
      scan_mode: scanMode,
      sast_tools: sastTools,
      config,
    });
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

  // 单任务 findings：自动翻页累积（page_size=100，与 console FindingsPage 口径一致）
  async listFindings(taskId: string): Promise<UnifiedFinding[]> {
    const all: UnifiedFinding[] = [];
    let cursor = '';
    for (let i = 0; i < 50; i++) {
      const page = await this.requestJson<FindingsPage>('GET', '/v1/findings', undefined, {
        query: { task_id: taskId, pagination: { page_size: 100, cursor } },
      });
      all.push(...(page.findings ?? []));
      if (!page.pagination?.has_next) break;
      cursor = page.pagination.next_cursor;
    }
    return all;
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
      });
    } catch (e) {
      this.markOffline(true); // 网络层失败 ≠ 会话失效：不清凭据，仅标离线
      throw e;
    }
    this.markOffline(false);
    if (!resp.ok) {
      this.tokens.clear();
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
        });
      } catch (e) {
        // 网络层失败（ECONNREFUSED/DNS/代理拒绝）：网关不可达。不清凭据（离线 ≠ 会话失效），
        // 仅翻转离线态供 UI 降级展示；后端宕机时isLoggedIn仍为 true 属预期语义
        this.markOffline(true);
        throw e;
      }
      this.markOffline(false); // 收到任何 HTTP 响应（含 4xx/5xx）= 网关可达
      if (resp.status === 429) {
        const errBody = (await resp.json().catch(() => ({}))) as { retry_after?: number };
        const retryAfter = Number(errBody.retry_after);
        this.rateLimitUntil = backoffMs(Number.isFinite(retryAfter) ? retryAfter : undefined, Date.now());
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
