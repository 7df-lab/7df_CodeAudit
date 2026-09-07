// 外部接口契约锚定（docs/external-interfaces.md [E-xx] ↔ 本文件用例双向追溯）。
// 纪律：断言"形状本身"（方法/URL/包装层/字段域/超时），不只断言"不抛错"——
// 形状漂移（如 ADR-200 上传响应变更）必须在此当次红。
import { describe, expect, it } from 'vitest';
import {
  createProject,
  createTask,
  getProject,
  getProjectConfig,
  getProjects,
  getReportContent,
  getSourceFile,
  getTools,
  updateProjectConfig,
  uploadArchive,
} from '../api/client';
import { httpError, useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

const gateway = useFakeGateway({
  'GET /v1/projects': { projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } },
  'POST /v1/projects': { project_id: 'p-new', name: 'A', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
  'GET /v1/projects/p1': { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p1/config': { project_id: 'p1', config: {} },
  'PUT /v1/projects/p1/config': { project_id: 'p1', config: { upload_file_id: 'f1' } },
  'POST /v1/uploads/archive': { upload_id: 'up-1', file_id: 'file-1', file_path: 'uploads/up-1/src.zip', size_bytes: 2 },
  'POST /v1/tasks': { task_id: 't-1' },
  'GET /v1/tools': { tools: [] },
  'GET /v1/reports/:reportId/download': (ctx: HandlerCtx) =>
    ctx.params.reportId === 'r-html' ? '<html><body>报告</body></html>' : '{"summary":{"total_findings":3}}',
  'GET /v1/tasks/:taskId/source-file': (ctx: HandlerCtx) => {
    if (ctx.query.get('path') === 'missing.py') httpError(404, { error: 'source root unavailable' });
    return { path: 'app.py', content: 'a\nb', total_lines: 2, bytes: 3, root_via: 'upload_link', resolved_via: 'exact' };
  },
});

describe('E-12 getProjects 分页参数（E-00a JSON 序列化 + 空游标保留）', () => {
  it('缺省分页：不发 pagination；给 page_size：cursor 缺省补空串', async () => {
    await getProjects();
    expect(gateway.requests[0].query).toBe('');
    await getProjects({ page_size: 10 });
    expect(gateway.requests[1].query).toBe(`pagination=${encodeURIComponent('{"page_size":10,"cursor":""}')}`);
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
  it('FormData 字段名 file；timeout 120s；响应 UploadArchiveResponse 直传', async () => {
    const file = new File(['PK'], 'src.zip', { type: 'application/zip' });
    const resp = await uploadArchive(file);
    const req = gateway.requests.find((r) => r.url === '/v1/uploads/archive')!;
    expect(req.method).toBe('POST');
    // 非字符串 body 原样进日志——multipart 时即 FormData 本体（字段名 file 是 E-11 契约）
    expect(req.body).toBeInstanceOf(FormData);
    expect((req.body as FormData).get('file')).toBe(file);
    expect(req.config.timeout).toBe(120_000);
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
