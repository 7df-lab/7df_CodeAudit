// 外部接口契约锚定（docs/external-interfaces.md [E-xx] ↔ 本文件用例双向追溯）。
// 纪律：断言"形状本身"（方法/URL/包装层/字段域/超时），不只断言"不抛错"——
// 形状漂移（如 ADR-200 上传响应变更）必须在此当次红。
import { describe, expect, it } from 'vitest';
import {
  createOrchestration,
  createProject,
  createWorkspace,
  deprecateOrchestration,
  listOrchestrations,
  updateOrchestration,
  deleteWorkspace,
  getWorkspaces,
  getInferenceProviders,
  getInferenceRoute,
  setInferenceRoute,
  createTask,
  getProject,
  getProjectConfig,
  getProjects,
  listAllProjects,
  listAllTasks,
  getReportContent,
  getSourceFile,
  probeSourceFile,
  getTools,
  updateProject,
  updateProjectConfig,
  uploadArchive,
  listInviteCodes,
  createInviteCode,
  revokeInviteCode,
  deleteInviteCode,
  getRegistrationPolicy,
} from '../api/client';
import { httpError, useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {
  'GET /v1/projects': { projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } },
  'POST /v1/projects': { project_id: 'p-new', name: 'A', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
  'GET /v1/projects/p1': { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p1/config': { project_id: 'p1', config: {} },
  'PUT /v1/projects/p1/config': { project_id: 'p1', config: { upload_file_id: 'f1' } },
  'PUT /v1/projects/p1': { project_id: 'p1', name: 'Demo', repo_url: 'https://git.new/x', default_branch: 'main', default_scan_mode: '', created_at: null },
  'POST /v1/uploads/archive': { upload_id: 'up-1', file_id: 'file-1', file_path: 'uploads/up-1/src.zip', size_bytes: 2 },
  'POST /v1/tasks': { task_id: 't-1' },
  'GET /v1/tools': { tools: [] },
  'GET /v1/reports/:reportId/download': (ctx: HandlerCtx) =>
    ctx.params.reportId === 'r-html' ? '<html><body>报告</body></html>' : '{"summary":{"total_findings":3}}',
  'GET /v1/tasks/:taskId/source-file': (ctx: HandlerCtx) => {
    if (ctx.query.get('path') === 'missing.py') httpError(404, { error: 'source root unavailable' });
    // 文件级未命中（engine resolve_only 口径的 404 文案）：存在性探测判 missing 的唯一依据
    if (ctx.query.get('path') === 'ghost.py') httpError(404, { error: 'root=uploads_unpacked: file not found in project: ghost.py' });
    return { path: 'app.py', content: 'a\nb', total_lines: 2, bytes: 3, root_via: 'upload_link', resolved_via: 'exact' };
  },
};
const gateway = useFakeGateway(routes);

describe('E-12 getProjects 分页参数（E-00a JSON 序列化 + 空游标保留）', () => {
  it('默认分页：不发 pagination；给 page_size：cursor 默认补空串', async () => {
    await getProjects();
    expect(gateway.requests[0].query).toBe('');
    await getProjects({ pagination: { page_size: 10 } });
    expect(gateway.requests[1].query).toBe(`pagination=${encodeURIComponent('{"page_size":10,"cursor":""}')}`);
  });
});

// B5-P2-7：项目下拉/索引全量获取——此前 getProjects() 默认页
// 只拿最新 20 条（服务端默认 20 上限 100），项目 >20 后旧项目在下拉/筛选/深链预选中
// 永远不可达。listAllProjects 循环翻页（服务端 project handler 恒发精确 has_next，循环
// 可靠终止；10 页熔断防异常 has_next 恒真打爆）。fakeGateway 每文件单 adapter——
// 覆写文件级 routes 条目（B3AuditFixes 同法），用后还原。
describe('B5-P2-7 listAllProjects 循环翻页', () => {
  const projectOf = (i: number) => ({
    project_id: `p${i}`, name: `P${i}`, repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null,
  });

  it('两页合并 101 条：恰 2 次请求，第二发携带 page_size=100 + cursor=100', async () => {
    const saved = routes['GET /v1/projects'];
    routes['GET /v1/projects'] = (ctx: HandlerCtx) => {
      const { cursor } = JSON.parse(ctx.query.get('pagination') ?? '{"cursor":""}');
      if (cursor === '') {
        return {
          projects: Array.from({ length: 100 }, (_, i) => projectOf(i + 1)),
          pagination: { next_cursor: '100', has_next: true, total: 101 },
        };
      }
      return { projects: [projectOf(101)], pagination: { next_cursor: '', has_next: false, total: 101 } };
    };
    try {
      const before = gateway.requests.length;
      const all = await listAllProjects();
      expect(all).toHaveLength(101);
      expect(all[99].project_id).toBe('p100');
      expect(all[100].project_id).toBe('p101');
      const hits = gateway.requests.slice(before).filter((r) => r.url === '/v1/projects');
      expect(hits).toHaveLength(2);
      expect(hits[1].query).toContain(encodeURIComponent('"page_size":100'));
      expect(hits[1].query).toContain(encodeURIComponent('"cursor":"100"'));
    } finally {
      routes['GET /v1/projects'] = saved;
    }
  });

  it('has_next 恒真 → 10 页熔断（防异常服务端打爆）', async () => {
    const saved = routes['GET /v1/projects'];
    routes['GET /v1/projects'] = () => ({
      projects: [projectOf(999)],
      pagination: { next_cursor: '1', has_next: true, total: 999999 },
    });
    try {
      const before = gateway.requests.length;
      await listAllProjects();
      expect(gateway.requests.slice(before).filter((r) => r.url === '/v1/projects')).toHaveLength(10);
    } finally {
      routes['GET /v1/projects'] = saved;
    }
  });
});

// 2026-10-08 统计冒充总数修复：任务域全量翻页——服务端按 proto 契约钳 page_size
// 上限 100（此前前端请求 200 被静默截断、页长冒充总数，<host-id> 实录 569 显示 100）。
// task_service 仅在有下页时填 Pagination（B5-P1-2 同源），末页无 pagination=循环终止。
describe('listAllTasks 循环翻页（统计带全量口径）', () => {
  const taskOf = (i: number, status = 'TASK_STATUS_COMPLETED') => ({
    task_id: `t${i}`, project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [],
    status, stages: [], created_at: null, updated_at: null, error_message: '', retry_count: 0,
  });

  it('两页合并 120 条：page_size 恒 100（不越服务端上限）、第二页 cursor=100、末页无 pagination 键即停', async () => {
    const saved = routes['GET /v1/tasks'];
    routes['GET /v1/tasks'] = (ctx: HandlerCtx) => {
      const { cursor } = JSON.parse(ctx.query.get('pagination') ?? '{"cursor":"0"}');
      if (cursor === '0') {
        return {
          tasks: Array.from({ length: 100 }, (_, i) => taskOf(i + 1)),
          pagination: { next_cursor: '100', has_next: true, total: 120 },
        };
      }
      // 末页真实形态：无 pagination 键（服务端 unset message 字段）
      return { tasks: Array.from({ length: 20 }, (_, i) => taskOf(101 + i, 'TASK_STATUS_FAILED')) };
    };
    try {
      const before = gateway.requests.length;
      const all = await listAllTasks();
      expect(all).toHaveLength(120);
      expect(all.filter((t) => t.status === 'TASK_STATUS_FAILED')).toHaveLength(20); // 跨页聚合=全集口径
      const hits = gateway.requests.slice(before).filter((r) => r.url === '/v1/tasks');
      expect(hits).toHaveLength(2);
      expect(hits.every((r) => r.query.includes(encodeURIComponent('"page_size":100')))).toBe(true); // 契约上限，永不请求 200
      expect(hits[1].query).toContain(encodeURIComponent('"cursor":"100"'));
    } finally {
      routes['GET /v1/tasks'] = saved;
    }
  });

  it('has_next 恒真 → 30 页熔断（防异常服务端打爆）', async () => {
    const saved = routes['GET /v1/tasks'];
    routes['GET /v1/tasks'] = () => ({
      tasks: [taskOf(999)],
      pagination: { next_cursor: '1', has_next: true, total: 999999 },
    });
    try {
      const before = gateway.requests.length;
      await listAllTasks();
      expect(gateway.requests.slice(before).filter((r) => r.url === '/v1/tasks')).toHaveLength(30);
    } finally {
      routes['GET /v1/tasks'] = saved;
    }
  });
});

describe('E-13/E-16 项目创建与配置写请求体包装', () => {
  it('createProject 包装为 {project: payload}（proto L844）；repo_url 可省略', async () => {
    await createProject({ name: 'A', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY' });
    const body = gateway.requests[0].body as { project: Record<string, unknown> };
    expect(gateway.requests[0].method).toBe('POST');
    expect(Object.keys(body)).toEqual(['project']);
    expect(body.project).toMatchObject({ name: 'A', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY' });
    expect('repo_url' in body.project).toBe(false);
  });

  it('updateProjectConfig 双层包装 {config:{project_id, config}}——扁平体会被 protojson 丢字段（E-16）', async () => {
    await updateProjectConfig('p1', { upload_file_id: 'f1' });
    const req = gateway.requests.find((r) => r.method === 'PUT')!;
    expect(req.url).toBe('/v1/projects/p1/config');
    const body = req.body as { config: { project_id: string; config: Record<string, string> } };
    expect(body.config.project_id).toBe('p1');
    expect(body.config.config).toEqual({ upload_file_id: 'f1' });
  });

  it('updateProject 双层包装 {project} 且钉 path id——服务端整体替换无字段合并，调用方须 GET 全量回写（E-58）', async () => {
    await updateProject('p1', {
      project_id: 'WRONG', // 错位 body：客户端与网关同口径钉死为 URL 段（R73/R91 防线）
      name: 'Demo', repo_url: 'https://git.new/x', default_branch: 'main', default_scan_mode: '', created_at: null,
    });
    const req = gateway.requests.find((r) => r.method === 'PUT')!;
    expect(req.url).toBe('/v1/projects/p1');
    const body = req.body as { project: Record<string, unknown> };
    expect(Object.keys(body)).toEqual(['project']);
    expect(body.project.project_id).toBe('p1');
    expect(body.project.repo_url).toBe('https://git.new/x');
  });
});

describe('E-14/E-15 响应裸形直传（无包装）', () => {
  it('getProject 返回裸 Project；getProjectConfig 返回 {project_id, config}', async () => {
    const p = await getProject('p1');
    expect(p).toEqual({ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null });
    const c = await getProjectConfig('p1');
    expect(Object.keys(c).sort()).toEqual(['config', 'project_id']);
  });
});

describe('E-11 uploadArchive multipart 契约', () => {
  it('FormData 字段名 file；timeout 300s（B4-3 慢速上行大包）；响应 UploadArchiveResponse 直传', async () => {
    const file = new File(['PK'], 'src.zip', { type: 'application/zip' });
    const resp = await uploadArchive(file);
    const req = gateway.requests.find((r) => r.url === '/v1/uploads/archive')!;
    expect(req.method).toBe('POST');
    // 非字符串 body 原样进日志——multipart 时即 FormData 本体（字段名 file 是 E-11 契约）
    expect(req.body).toBeInstanceOf(FormData);
    expect((req.body as FormData).get('file')).toBe(file);
    expect(req.config.timeout).toBe(300_000);
    expect(resp.file_id).toBe('file-1');
    expect(resp).toMatchObject({ upload_id: 'up-1', file_path: 'uploads/up-1/src.zip', size_bytes: 2 });
  });
});

describe('E-19/E-32 createTask 请求体直传 + tools 形状', () => {
  it('createTask body 无包装（config/upload_file_id 键域见 E-19）', async () => {
    await createTask({ project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep'], config: { upload_file_id: 'f1' } });
    expect(gateway.requests[0].body).toEqual({
      project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep'], config: { upload_file_id: 'f1' },
    });
    await getTools();
    expect(gateway.requests[1].url).toBe('/v1/tools');
  });
});

describe('E-29 getReportContent 内容分型（< 前缀嗅探 + 保原文）', () => {
  it('JSON 正文：format=json 且内容原样（不被 JSON.parse/再序列化破坏）', async () => {
    const r = await getReportContent('r-json');
    expect(r).toEqual({ format: 'json', content: '{"summary":{"total_findings":3}}' });
  });
  it('HTML 正文：format=html', async () => {
    const r = await getReportContent('r-html');
    expect(r.format).toBe('html');
    expect(r.content).toContain('<html>');
  });
});

describe('E-24 getSourceFile 请求形状与错误详情提取', () => {
  it('path 走 query；成功响应类型直传', async () => {
    const r = await getSourceFile('t-9', 'app.py');
    expect(gateway.requests[0].query).toContain('path=app.py');
    expect(r).toMatchObject({ path: 'app.py', total_lines: 2, root_via: 'upload_link', resolved_via: 'exact' });
  });
  it('失败时 Error.message = 服务端 {error} 详情（降级横幅可读），非 axios 通用语', async () => {
    await expect(getSourceFile('t-9', 'missing.py')).rejects.toThrow('source root unavailable');
  });
});

describe('E-24b probeSourceFile 三态（resolve_only 存在性探测, 2026-09-13 误挂接根治）', () => {
  it('命中：200 → exists；请求带 path 与 resolve_only=1', async () => {
    const r = await probeSourceFile('t-9', 'app.py');
    expect(r).toBe('exists');
    const req = gateway.requests.filter((x) => x.url === '/v1/tasks/t-9/source-file' && x.query.includes('resolve_only=1')).pop();
    expect(req?.query).toContain('path=app.py');
  });
  it('404 且 {error} 含 file not found in project → missing（文件级未命中）', async () => {
    expect(await probeSourceFile('t-9', 'ghost.py')).toBe('missing');
  });
  it('根级 404（源根不可解析）≠ 文件不存在 → unknown（fail-open 防误判幻觉引用）', async () => {
    expect(await probeSourceFile('t-9', 'missing.py')).toBe('unknown');
  });
});

// 切片3b（team 上车）：workspace 查询参数透传——六个推理端点统一 ?workspace=
// （engine 转码 query 覆盖 body 同名，client 侧最简形态=纯 query）；默认不发
// （=全局默认 default，向后兼容）。getWorkspaces 为 Provider 页选择器数据源。
describe('切片3b workspace 参数透传', () => {
  beforeAll(() => {
    routes['GET /v1/workspaces'] = { workspaces: [{ name: 'default', phase: 'Active', labels: {} }, { name: 'team-a', phase: 'Active', labels: { team: 'a' } }] };
    routes['GET /v1/inference/providers'] = { providers: [] };
    routes['GET /v1/inference/route'] = { provider: '', model: '', version: 0 };
    routes['PUT /v1/inference/route'] = { provider: 'p', model: 'm', version: 1 };
  });

  it('getWorkspaces → GET /v1/workspaces 原样 query 为空', async () => {
    const r = await getWorkspaces();
    expect(r.workspaces.map((w) => w.name)).toEqual(['default', 'team-a']);
    expect(gateway.requests.at(-1)?.query).toBe('');
  });

  it('getInferenceProviders/getInferenceRoute：默认不带 workspace，给值带 ?workspace=', async () => {
    await getInferenceProviders();
    expect(gateway.requests.at(-1)?.query).toBe('');
    await getInferenceProviders('team-a');
    expect(gateway.requests.at(-1)?.query).toBe('workspace=team-a');
    await getInferenceRoute('team-a');
    expect(gateway.requests.at(-1)?.query).toBe('workspace=team-a');
  });

  it('setInferenceRoute：workspace 走 query（转码层 query 覆盖 body 同型）', async () => {
    await setInferenceRoute({ provider: 'p', model: 'm' }, 'team-a');
    const req = gateway.requests.at(-1);
    expect(req?.query).toBe('workspace=team-a');
    expect(req?.method).toBe('PUT');
  });
});

// 切片3b 扩面：工作区写通道——create/delete 的错误详情透出（409 资源守卫原文）。
describe('切片3b 工作区管理写通道', () => {
  beforeAll(() => {
    routes['POST /v1/workspaces'] = { name: 'team-c', phase: 'Active', labels: {} };
    routes['DELETE /v1/workspaces/:name'] = (ctx: HandlerCtx) => {
      if (ctx.params.name === 'busy') httpError(409, { error: "workspace 'busy' still contains resources: sandbox, service" });
      return { name: ctx.params.name, deleted: true };
    };
  });

  it('createWorkspace POST /v1/workspaces', async () => {
    const r = await createWorkspace({ name: 'team-c' });
    expect(r.name).toBe('team-c');
    expect(gateway.requests.at(-1)?.method).toBe('POST');
  });

  it('deleteWorkspace 409 资源守卫原文透出（非 axios 通用文案）', async () => {
    await expect(deleteWorkspace('busy')).rejects.toThrow(/still contains resources/);
  });
});

describe('编排 CRUD 契约锚定（E-50..E-53，2026-09-21 编排 CRUD 批；批1-Step4 补锚）', () => {
  const orchInfo = { name: 'audit-x', latest_revision: 1, spec_yaml: 'apiVersion: codeaudit.pack/v0\n', spec_hash: 'a'.repeat(64), execution: 'graph', description: '', deprecated: false, readonly: false, updated_at: '2026-09-22T00:00:00Z' };
  beforeAll(() => {
    routes['GET /v1/orchestrations'] = { orchestrations: [orchInfo] };
    routes['POST /v1/orchestrations'] = (ctx: HandlerCtx) => {
      const body = (ctx.body ?? {}) as { name?: string; spec_yaml?: string };
      if (!body.name || !body.spec_yaml) httpError(400, { error: 'name 与 spec_yaml 必填' });
      return { ...orchInfo, name: body.name };
    };
    routes['PUT /v1/orchestrations/:name'] = (ctx: HandlerCtx) => ({ ...orchInfo, latest_revision: 2, name: ctx.params.name });
    routes['DELETE /v1/orchestrations/:name'] = (ctx: HandlerCtx) => {
      if (ctx.params.name === 'builtin') httpError(409, { error: '编排 "builtin" 为内置只读目录——不可停用' });
      return { ...orchInfo, name: ctx.params.name, deprecated: !ctx.query.get('unde_precate') };
    };
  });

  it('E-50 listOrchestrations：query 形状 include_deprecated/include_history（默认不发）', async () => {
    const r = await listOrchestrations({});
    expect(r.orchestrations[0].name).toBe('audit-x');
    expect(gateway.requests.at(-1)?.query).toBe('');
    await listOrchestrations({ include_deprecated: true, include_history: true });
    const q = gateway.requests.at(-1)?.query ?? '';
    expect(q).toContain('include_deprecated=true');
    expect(q).toContain('include_history=true');
  });

  it('E-51 createOrchestration：POST body={name, spec_yaml}', async () => {
    const r = await createOrchestration('audit-new', 'apiVersion: codeaudit.pack/v0\n');
    expect(r.name).toBe('audit-new');
    const req = gateway.requests.at(-1);
    expect(req?.method).toBe('POST');
    expect(req?.url).toBe('/v1/orchestrations');
    expect((req?.body as { name: string; spec_yaml: string }).spec_yaml).toContain('codeaudit.pack/v0');
  });

  it('E-52 updateOrchestration：PUT /v1/orchestrations/<encodeURIComponent(name)> 路径 name 权威', async () => {
    const r = await updateOrchestration('audit x/1', 'apiVersion: codeaudit.pack/v0\n');
    expect(r.latest_revision).toBe(2);
    const req = gateway.requests.at(-1);
    expect(req?.method).toBe('PUT');
    expect(req?.url).toBe('/v1/orchestrations/audit%20x%2F1');
  });

  it('E-53 deprecateOrchestration：DELETE + ?unde_precate=true 恢复；内置 409', async () => {
    const r = await deprecateOrchestration('audit-x');
    expect(gateway.requests.at(-1)?.url).toBe('/v1/orchestrations/audit-x');
    expect(r.deprecated).toBe(true);
    const r2 = await deprecateOrchestration('audit-x', true);
    expect(gateway.requests.at(-1)?.query).toContain('unde_precate=true');
    expect(r2.deprecated).toBe(false);
    // 409 details 原文由页面层 apiErrorMessage 提取（client.ts:169 单点）——本层锚状态码
    await expect(deprecateOrchestration('builtin')).rejects.toMatchObject({ response: { status: 409 } });
  });
});

// V2.2 邀请码管理（ADR-255）：五个类型化端点——形状单点锚定 client.ts（ADR-203），
// external-interfaces E-59..E-63 双向追溯锚。
describe('V2.2 邀请码端点契约', () => {
  beforeAll(() => {
    routes['GET /v1/invites'] = { codes: [], config_codes: ['CODEAUDIT-X'], pagination: { has_next: false } };
    routes['POST /v1/invites'] = { id: 'invite-1', code: 'CODEAUDIT-AAAAA-BBBBB', max_uses: 1, used_count: 0, expires_at: null, state: 'active', note: '', created_by: 'u-1', created_at: null };
    routes['POST /v1/invites/invite-1/revoke'] = { id: 'invite-1', state: 'revoked' };
    routes['DELETE /v1/invites/invite-1'] = {};
    routes['GET /v1/auth/registration-policy'] = { mode: 'invitation' };
  });

  it('listInviteCodes 透传 include_terminal 与游标', async () => {
    const r1 = await listInviteCodes({ includeTerminal: true });
    expect(r1.config_codes).toEqual(['CODEAUDIT-X']);
    routes['GET /v1/invites'] = { codes: [], pagination: { has_next: true, next_cursor: '20' } };
    await listInviteCodes({ cursor: '20' });
    expect(decodeURIComponent(gateway.requests.at(-1)?.query ?? '')).toContain('pagination={"cursor":"20"}');
  });

  it('createInviteCode/revoke/delete/policy 形状直通', async () => {
    const created = await createInviteCode({ max_uses: 0, never_expires: true });
    expect(created.code).toBe('CODEAUDIT-AAAAA-BBBBB');
    expect((await revokeInviteCode('invite-1')).state).toBe('revoked');
    await expect(deleteInviteCode('invite-1')).resolves.toBeUndefined();
    expect((await getRegistrationPolicy()).mode).toBe('invitation');
  });
});
