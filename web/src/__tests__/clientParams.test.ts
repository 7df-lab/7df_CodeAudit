// @vitest-environment node
// ADR-155 回归锁：分页参数必须按网关 decodeQuery 的 JSON 风格序列化
// （?pagination={"page_size":20,"cursor":"5"}），杜绝 axios 默认 bracket 风格导致的翻页失效。
import { afterEach, describe, expect, it } from 'vitest';
import type { AxiosResponse, InternalAxiosRequestConfig } from 'axios';
import { api } from '../api/client';

const serialize = (params: unknown): string =>
  (api.defaults.paramsSerializer as { serialize: (p: unknown) => string }).serialize(params);

// 捕获实际发出的 URL（query 序列化结果）
let capturedUrl = '';
const captureAdapter = async (config: InternalAxiosRequestConfig): Promise<AxiosResponse> => {
  const qs = serialize(config.params ?? {});
  capturedUrl = `${config.url}${qs ? '?' + qs : ''}`;
  return { data: { ok: true }, status: 200, statusText: 'OK', headers: {}, config };
};

describe('api client 分页参数序列化（ADR-155）', () => {
  afterEach(() => {
    (api.defaults as { adapter?: unknown }).adapter = undefined;
  });

  it('嵌套 pagination 对象 → JSON 编码（网关 decodeQuery 可解析）', () => {
    const qs = serialize({ pagination: { page_size: 20, cursor: '5' } });
    expect(qs).toBe(`pagination=${encodeURIComponent('{"page_size":20,"cursor":"5"}')}`);
    expect(qs).not.toContain('%5B'); // 不得出现 bracket 风格 pagination[...]
  });

  it('标量参数照常（task_id 不走 JSON）', () => {
    const qs = serialize({ task_id: 't-1', pagination: { page_size: 100 } });
    expect(qs).toBe(`task_id=t-1&pagination=${encodeURIComponent('{"page_size":100}')}`);
  });

  it('undefined/null 参数被剔除；JSON 内空游标保留（首屏语义）', () => {
    const qs = serialize({ task_id: undefined, verdict: null, pagination: { page_size: 3, cursor: '' } });
    expect(qs).not.toContain('task_id');
    expect(qs).not.toContain('verdict');
    expect(qs).toBe(`pagination=${encodeURIComponent('{"page_size":3,"cursor":""}')}`);
  });

  it('端到端：api.get 实际请求 URL 为 JSON 风格（服务端可翻页）', async () => {
    (api.defaults as { adapter?: unknown }).adapter = captureAdapter;
    await api.get('/v1/tasks', { params: { pagination: { page_size: 20, cursor: '20' } } });
    expect(capturedUrl.startsWith('/v1/tasks?pagination=')).toBe(true);
    expect(capturedUrl).toBe(`/v1/tasks?pagination=${encodeURIComponent('{"page_size":20,"cursor":"20"}')}`);
  });
});
