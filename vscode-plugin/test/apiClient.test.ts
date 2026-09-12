import * as assert from 'assert';
import { CodeAuditClient, ApiError, encodeQuery, REQUEST_TIMEOUT_MS, UPLOAD_TIMEOUT_MS, type FetchLike, type TokenStore } from '../src/apiClient';

function makeStore(access = '', refresh = ''): TokenStore & { access: string; refresh: string } {
  const s = { access, refresh };
  return {
    get access() { return s.access; },
    get refresh() { return s.refresh; },
    getAccessToken: () => s.access,
    getRefreshToken: () => s.refresh,
    setTokens: (a: string, r?: string) => { s.access = a; if (r) s.refresh = r; },
    clear: () => { s.access = ''; s.refresh = ''; },
  } as never;
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

describe('encodeQuery（ADR-155 JSON 风格查询参数）', () => {
  it('对象值 JSON 编码、标量照常、空值跳过', () => {
    const q = encodeQuery({ task_id: 't1', pagination: { page_size: 100, cursor: '' }, unused: undefined });
    const sp = new URLSearchParams(q.slice(1));
    assert.strictEqual(sp.get('task_id'), 't1');
    assert.strictEqual(sp.get('pagination'), '{"page_size":100,"cursor":""}');
    assert.ok(!sp.has('unused'));
  });
});

describe('CodeAuditClient', () => {
  it('login 成功后保存 access+refresh', async () => {
    const store = makeStore();
    const fetchFn: FetchLike = async (url) => {
      assert.ok(String(url).endsWith('/v1/auth/login'));
      return jsonResponse(200, { access_token: 'A1', refresh_token: 'R1', expires_in_s: 1800 });
    };
    const c = new CodeAuditClient('http://x:8080/', store as TokenStore, fetchFn);
    const resp = await c.login('u', 'p');
    assert.strictEqual(resp.access_token, 'A1');
    assert.strictEqual(store.access, 'A1');
    assert.strictEqual(store.refresh, 'R1');
    assert.strictEqual(c.baseUrl, 'http://x:8080'); // 尾斜杠规范化
  });

  it('401 触发单飞刷新并重放原请求（并发共享一次刷新）', async () => {
    const store = makeStore('stale', 'R0');
    let refreshCalls = 0;
    let lastAuth = '';
    const fetchWithAuth: FetchLike = async (url, init) => {
      const u = String(url);
      if (u.endsWith('/v1/auth/refresh')) {
        refreshCalls++;
        return jsonResponse(200, { access_token: 'A2', refresh_token: 'R2', expires_in_s: 1800 });
      }
      lastAuth = String((init?.headers as Record<string, string>)?.Authorization ?? '');
      if (lastAuth === 'Bearer stale') return jsonResponse(401, { error: 'expired' });
      return jsonResponse(200, { projects: [{ project_id: 'p1', name: 'n' }] });
    };
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchWithAuth);
    const [a, b] = await Promise.all([c.listProjects(), c.listProjects()]);
    assert.strictEqual(a.length, 1);
    assert.strictEqual(b.length, 1);
    assert.strictEqual(refreshCalls, 1, '并发两个 401 应只触发一次刷新');
    assert.strictEqual(lastAuth, 'Bearer A2');
  });

  it('刷新失败（无 refresh token）抛 401 且清空会话', async () => {
    const store = makeStore('stale', '');
    const fetchFn: FetchLike = async () => jsonResponse(401, {});
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    await assert.rejects(() => c.listProjects(), ApiError);
    assert.strictEqual(store.access, '');
  });

  it('listFindings 翻页累积（has_next/next_cursor）', async () => {
    const store = makeStore('ok', 'R');
    const pages = [
      { findings: [{ finding_id: 'f1' }, { finding_id: 'f2' }], pagination: { next_cursor: 'c2', has_next: true, total: 3 } },
      { findings: [{ finding_id: 'f3' }], pagination: { next_cursor: '', has_next: false, total: 3 } },
    ];
    const seenUrls: string[] = [];
    const fetchFn: FetchLike = async (url) => {
      seenUrls.push(String(url));
      return jsonResponse(200, pages.shift()!);
    };
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    const findings = await c.listFindings('t1');
    assert.strictEqual(findings.length, 3);
    assert.strictEqual(seenUrls.length, 2);
    assert.ok(seenUrls[1].includes('c2'), '第二页应携带 cursor');
    assert.ok(seenUrls[0].includes(encodeURIComponent('{"page_size":100,"cursor":""}')), 'JSON 风格分页参数');
  });

  it('listTasks 翻页累积（project_id 透传 + has_next/next_cursor 跟页）', async () => {
    const store = makeStore('ok', 'R');
    const pages = [
      { tasks: [{ task_id: 't1' }], pagination: { next_cursor: '100', has_next: true, total: 2 } },
      { tasks: [{ task_id: 't2' }], pagination: { next_cursor: '', has_next: false, total: 2 } },
    ];
    const seenUrls: string[] = [];
    const fetchFn: FetchLike = async (url) => {
      seenUrls.push(String(url));
      return jsonResponse(200, pages.shift()!);
    };
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    const tasks = await c.listTasks('p1');
    assert.strictEqual(tasks.length, 2);
    assert.strictEqual(seenUrls.length, 2);
    assert.ok(seenUrls[0].includes('project_id=p1'), 'project_id 透传');
    assert.ok(seenUrls[1].includes('cursor%22%3A%22100%22') || seenUrls[1].includes('"cursor":"100"'),
      '第二页应携带游标 100');
  });

  it('429 记录退避窗口（retry_after 钳位 5~60s）', async () => {
    const store = makeStore('ok', 'R');
    const fetchFn: FetchLike = async () => jsonResponse(429, { retry_after: 2 }); // 低于下限 → 5s
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    await assert.rejects(() => c.listProjects());
    assert.ok(c.rateLimitUntil > Date.now() + 4_000);
    assert.ok(c.rateLimitUntil <= Date.now() + 61_000);
  });

  it('429 响应体单次读取：ApiError 携带完整 body，retry_after 正常解析（回归锁：json+text 双读丢 body）', async () => {
    const store = makeStore('ok', 'R');
    const fetchFn: FetchLike = async () =>
      new Response(JSON.stringify({ retry_after: 7, error: 'rate limited, slow down' }), { status: 429, headers: { 'Content-Type': 'application/json' } });
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    const err = await c.listProjects().then(() => assert.fail('应抛 429'), (e: unknown) => e as ApiError);
    assert.strictEqual(err.status, 429);
    assert.match(err.message, /rate limited, slow down/, '错误消息保留响应体（原实现 body 双读后被吞成空串）');
    assert.match(String(err.body ?? ''), /rate limited/);
    assert.ok(c.rateLimitUntil > Date.now() + 6_000, 'retry_after=7 已从单次读取的 body 解析');
    assert.ok(c.rateLimitUntil <= Date.now() + 61_000);
  });

  it('taskSnapshot 增量游标：logs_after/ai_cursor 进 query，空游标不发参数', async () => {
    const store = makeStore('ok', 'R');
    const seenUrls: string[] = [];
    const snap = { task: { task_id: 't1', status: 'TASK_STATUS_RUNNING', stages: [] }, progress: null, logs: { logs: [] }, ai: null };
    const fetchFn: FetchLike = async (url) => {
      seenUrls.push(String(url));
      return jsonResponse(200, snap);
    };
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    await c.taskSnapshot('t1');
    await c.taskSnapshot('t1', { logsAfter: '42', aiCursor: 1024 });
    assert.ok(!seenUrls[0].includes('logs_after'), '无游标不带参数');
    assert.ok(seenUrls[1].includes('logs_after=42'));
    assert.ok(seenUrls[1].includes('ai_cursor=1024'));
  });

  it('cancelTask POST 到 /v1/tasks/{id}/cancel', async () => {
    const store = makeStore('ok', 'R');
    let hit = '';
    const fetchFn: FetchLike = async (url, init) => {
      hit = `${init?.method} ${String(url)}`;
      return jsonResponse(200, {});
    };
    const c = new CodeAuditClient('http://x', store as TokenStore, fetchFn);
    await c.cancelTask('gw-1');
    assert.strictEqual(hit, 'POST http://x/v1/tasks/gw-1/cancel');
  });
});

describe('CodeAuditClient 连通性跟踪（offline 与 isLoggedIn 正交）', () => {
  function makeWatchedClient(store: TokenStore, fetchFn: FetchLike) {
    const events: boolean[] = [];
    const c = new CodeAuditClient('http://x', store, fetchFn, { onStateChange: () => events.push(c.offline) });
    return { c, events };
  }

  it('网络层抛错 → offline=true（通知一次），凭据不清（离线 ≠ 会话失效）', async () => {
    const store = makeStore('ok', 'R');
    let attempts = 0;
    const fetchFn: FetchLike = async () => {
      attempts++;
      throw new TypeError('fetch failed: ECONNREFUSED');
    };
    const { c, events } = makeWatchedClient(store as TokenStore, fetchFn);
    await c.listTools().then(() => assert.fail('应抛出'), (e) => assert.ok(/ECONNREFUSED/.test((e as Error).message)));
    assert.strictEqual(c.offline, true, '网络失败必须标记离线');
    assert.strictEqual(attempts, 1);
    assert.deepStrictEqual(events, [true], '翻转沿才通知');
    assert.strictEqual(store.getAccessToken(), 'ok', '离线不得清除凭据');
  });

  it('恢复可达 → offline=false 再通知；期间无翻转不重复通知', async () => {
    const store = makeStore('ok', 'R');
    let fail = true;
    const fetchFn: FetchLike = async () => {
      if (fail) throw new TypeError('fetch failed');
      return jsonResponse(200, { tools: [] });
    };
    const { c, events } = makeWatchedClient(store as TokenStore, fetchFn);
    await c.listTools().then(() => assert.fail('应抛出'), () => undefined);
    assert.deepStrictEqual(events, [true]);
    fail = false;
    await c.listTools();
    assert.strictEqual(c.offline, false);
    await c.listTools(); // 已在线：不再通知
    assert.deepStrictEqual(events, [true, false], '只在翻转沿通知');
  });

  it('收到 4xx/5xx 也算可达（离线仅指网络层不通）', async () => {
    const store = makeStore('ok', 'R');
    const fetchFn: FetchLike = async () => jsonResponse(500, { error: 'boom' });
    const { c } = makeWatchedClient(store as TokenStore, fetchFn);
    await c.listTools().then(() => assert.fail('应抛出'), (e) => assert.ok(e instanceof ApiError));
    assert.strictEqual(c.offline, false, '后端响应了错误码 ≠ 网关不可达');
  });

  it('doRefresh 网络失败：不清凭据只标离线；refresh 401：清凭据（会话失效）', async () => {
    const netFail = makeStore('stale', 'R');
    let fail = true;
    const c1 = new CodeAuditClient('http://x', netFail as TokenStore, async () => {
      if (fail) throw new TypeError('fetch failed');
      throw new Error('unreachable');
    });
    // 触发 401 → 单飞刷新 → 网络失败
    await c1.listTools().then(() => assert.fail('应抛出'), () => undefined);
    assert.strictEqual(c1.offline, true);
    assert.strictEqual(netFail.getRefreshToken(), 'R', '网络失败不清 refresh（恢复后可自动续期）');

    const http401 = makeStore('stale', 'R');
    const c2 = new CodeAuditClient('http://x', http401 as TokenStore, async () => jsonResponse(401, { error: 'bad refresh' }));
    await c2.listTools().then(() => assert.fail('应抛出'), () => undefined);
    assert.strictEqual(http401.getRefreshToken(), '', 'refresh 被拒=会话失效，凭据必须清');
    assert.strictEqual(c2.isLoggedIn(), false);
  });
});

describe('createTask 增量载荷（ADR-225 D5）', () => {
  it('全量调用不携带任何增量键（旧契约零变化）', async () => {
    let body: Record<string, unknown> | undefined;
    const fetchFn: FetchLike = async (_url, init) => {
      body = JSON.parse(String(init?.body));
      return jsonResponse(200, { task_id: 't1', status: 'TASK_STATUS_CREATED' });
    };
    const c = new CodeAuditClient('http://x', makeStore('A'), fetchFn);
    await c.createTask('p1', 'SCAN_MODE_PARALLEL', [], { upload_file_id: 'f1' });
    assert.ok(!('incremental' in (body ?? {})));
    assert.ok(!('git_anchor' in (body ?? {})));
    assert.strictEqual((body?.config as Record<string, string> | undefined)?.upload_file_id, 'f1');
  });

  it('增量调用携带 incremental/git_anchor/diff_hint（snake_case 契约）', async () => {
    let body: Record<string, unknown> | undefined;
    const fetchFn: FetchLike = async (_url, init) => {
      body = JSON.parse(String(init?.body));
      return jsonResponse(200, { task_id: 't2', status: 'TASK_STATUS_CREATED' });
    };
    const c = new CodeAuditClient('http://x', makeStore('A'), fetchFn);
    await c.createTask('p1', 'SCAN_MODE_PARALLEL', [], { upload_file_id: 'f2' }, {
      git_anchor: { commit: 'abc', branch: 'main', dirty: true, remote: 'git://x' },
      diff_hint: 'M\ta.py',
    });
    assert.strictEqual(body?.incremental, true);
    assert.deepStrictEqual(body?.git_anchor, { commit: 'abc', branch: 'main', dirty: true, remote: 'git://x' });
    assert.strictEqual(body?.diff_hint, 'M\ta.py');
    assert.ok(!('baseline_task_id' in (body ?? {}))); // 自动选定时省略（服务端 ADR-225 规则）
  });
});

describe('refresh 失败仅 401 清凭据（B2-3 回归锁）', () => {
  // 组合路由：业务请求带过期 access → 401 触发单飞刷新；refresh 端点按脚本返回 status
  const expiredThenRefresh = (refreshStatus: number): FetchLike => async (url) =>
    /\/v1\/auth\/refresh/.test(String(url))
      ? jsonResponse(refreshStatus, { error: 'refresh rejected' })
      : jsonResponse(401, { error: 'token expired' });

  it('refresh 502 → 抛错但凭据保留（网关瞬态故障不把用户登出）', async () => {
    const store = makeStore('stale', 'R');
    const c = new CodeAuditClient('http://x', store as TokenStore, expiredThenRefresh(502));
    await c.listTools().then(() => assert.fail('应抛出'), (e) => assert.ok(/refresh failed: 502/.test((e as Error).message)));
    assert.strictEqual(store.access, 'stale', 'access 不得被清');
    assert.strictEqual(store.refresh, 'R', 'refresh 不得被清');
    assert.strictEqual(c.isLoggedIn(), true, '瞬态失败期间会话仍视为存在（可用缓存 token 重试）');
  });

  it('refresh 429 → 抛错但凭据保留（限流不等于会话失效）', async () => {
    const store = makeStore('stale', 'R');
    const c = new CodeAuditClient('http://x', store as TokenStore, expiredThenRefresh(429));
    await c.listTools().then(() => assert.fail('应抛出'), (e) => assert.ok(/refresh failed: 429/.test((e as Error).message)));
    assert.strictEqual(store.access, 'stale');
    assert.strictEqual(store.refresh, 'R');
    assert.strictEqual(c.isLoggedIn(), true);
  });

  it('refresh 401 → 凭据清空（服务端明确拒绝 token，会话失效口径不变）', async () => {
    const store = makeStore('stale', 'R');
    const c = new CodeAuditClient('http://x', store as TokenStore, expiredThenRefresh(401));
    await c.listTools().then(() => assert.fail('应抛出'), () => undefined);
    assert.strictEqual(store.access, '');
    assert.strictEqual(store.refresh, '');
    assert.strictEqual(c.isLoggedIn(), false);
  });
});

describe('REST 超时注入（B2-7 回归锁）', () => {
  it('requestJson 注入 AbortSignal.timeout：普通请求 60s 档', async () => {
    const orig = AbortSignal.timeout.bind(AbortSignal);
    const seen: number[] = [];
    (AbortSignal as any).timeout = (ms: number) => { seen.push(ms); return orig(ms); };
    try {
      const fetchFn: FetchLike = async () => jsonResponse(200, { tools: [] });
      const c = new CodeAuditClient('http://x', makeStore('A'), fetchFn);
      await c.listTools();
      assert.deepStrictEqual(seen, [REQUEST_TIMEOUT_MS], '普通 JSON 请求走 REQUEST_TIMEOUT_MS 档');
      assert.strictEqual(REQUEST_TIMEOUT_MS, 60_000, '缺省 60s');
    } finally {
      (AbortSignal as any).timeout = orig;
    }
  });

  it('上传走 600s 长档（UPLOAD_TIMEOUT_MS）', async () => {
    const orig = AbortSignal.timeout.bind(AbortSignal);
    const seen: number[] = [];
    (AbortSignal as any).timeout = (ms: number) => { seen.push(ms); return orig(ms); };
    try {
      const fetchFn: FetchLike = async () => jsonResponse(200, { file_id: 'f1' });
      const c = new CodeAuditClient('http://x', makeStore('A'), fetchFn);
      await c.uploadArchive(new Blob(['zip-bytes']));
      assert.deepStrictEqual(seen, [UPLOAD_TIMEOUT_MS], 'multipart 上传走 UPLOAD_TIMEOUT_MS 长档');
      assert.strictEqual(UPLOAD_TIMEOUT_MS, 600_000, '缺省 600s');
    } finally {
      (AbortSignal as any).timeout = orig;
    }
  });

  it('doRefresh 裸 fetch 亦注入超时信号（刷新悬挂同样受控）', async () => {
    let captured: RequestInit | undefined;
    const store = makeStore('stale', 'R');
    const c = new CodeAuditClient('http://x', store as TokenStore, async (_url, init) => {
      captured = init;
      return jsonResponse(401, { error: 'expired' });
    });
    await c.listTools().then(() => assert.fail('应抛出'), () => undefined);
    assert.ok(captured?.signal instanceof AbortSignal, 'refresh 裸 fetch 必须带超时 signal');
  });
});
