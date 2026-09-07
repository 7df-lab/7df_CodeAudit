// 会话上下文契约（docs/internal-interfaces.md §3 [I-30..I-35]；外部 E-01..E-05）。
// 此前 session.tsx 零直测——login/logout 请求体形状（P-14）与 F5 静默续签均为未锚契约。
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionProvider, useSession } from '../auth/session';
import { TOKEN_KEY, clearSession } from '../api/client';
import { useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

type SessionApi = ReturnType<typeof useSession>;
let S: SessionApi | null = null;
let meAuthHeader = ''; // 最近一次 GET /v1/users/me 的 Authorization

const routes: Record<string, unknown> = {
  'POST /v1/auth/login': { access_token: 'acc-1', refresh_token: 'ref-1', expires_in_s: 1800 },
  'POST /v1/auth/register': { access_token: 'acc-r', refresh_token: 'ref-r', expires_in_s: 1800 },
  'POST /v1/auth/logout': {},
  'GET /v1/users/me': (ctx: HandlerCtx) => {
    meAuthHeader = String(ctx.config.headers?.Authorization ?? '');
    return { user_id: 'u-1', username: 'alice', email: 'a@x', role: 'ROLE_ADMIN' };
  },
};
const gateway = useFakeGateway(routes);

function Probe() {
  S = useSession();
  return <span data-testid="who">{S.user?.username ?? 'anon'}</span>;
}
function renderSession() {
  render(<SessionProvider><Probe /></SessionProvider>);
}
async function boot() {
  renderSession();
  await waitFor(() => expect(S?.booting).toBe(false));
}
function lastRequest(url: string) {
  const hits = gateway.requests.filter((r) => r.url === url);
  expect(hits.length).toBeGreaterThan(0);
  return hits[hits.length - 1];
}

beforeEach(() => {
  S = null;
  meAuthHeader = '';
  clearSession();
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe('I-32 login（E-01）', () => {
  it('凭证→令牌落位（内存 access + localStorage refresh）→ 携 Bearer 拉 me → user 就位', async () => {
    await boot();
    await S!.login('alice', 'pw');
    const login = lastRequest('/v1/auth/login');
    expect(login.body).toEqual({ username: 'alice', password: 'pw' });
    expect(meAuthHeader).toBe('Bearer acc-1');
    expect(localStorage.getItem(TOKEN_KEY)).toBe('ref-1');
    await waitFor(() => expect(screen.getByTestId('who').textContent).toBe('alice'));
  });
});

describe('I-33 register（E-02：空邀请码必须从 body 剔除）', () => {
  it('invite 为空串 → 请求体无 invite_code 键；注册即登录', async () => {
    await boot();
    await S!.register('bob', 'b@x', 'pw12345678', '');
    expect(lastRequest('/v1/auth/register').body).toEqual({ username: 'bob', email: 'b@x', password: 'pw12345678' });
    expect(localStorage.getItem(TOKEN_KEY)).toBe('ref-r');
    await waitFor(() => expect(screen.getByTestId('who').textContent).toBe('alice'));
  });

  it('invite 非空 → 原样携带', async () => {
    await boot();
    await S!.register('carol', 'c@x', 'pw12345678', 'XMAS');
    expect(lastRequest('/v1/auth/register').body).toMatchObject({ invite_code: 'XMAS' });
  });
});

describe('I-34 logout（E-04：必须携带 access_token；服务端失败也清会话）', () => {
  it('logout body 携带 access_token（空 body 后端恒 400——P-14），随后双清、user 置空', async () => {
    await boot();
    await S!.login('alice', 'pw');
    await S!.logout();
    const logout = lastRequest('/v1/auth/logout');
    expect(logout.body).toEqual({ access_token: 'acc-1' });
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    await waitFor(() => expect(screen.getByTestId('who').textContent).toBe('anon'));
  });

  it('logout 端点失败：promise reject 但 finally 仍清会话（登出不受服务端状态影响）', async () => {
    routes['POST /v1/auth/logout'] = () => {
      throw Object.assign(new Error('HTTP 500'), {
        isAxiosError: true,
        response: { status: 500, data: {}, headers: {}, config: {}, statusText: '500' },
      });
    };
    await boot();
    await S!.login('alice', 'pw');
    await expect(S!.logout()).rejects.toThrow('HTTP 500');
    await waitFor(() => expect(screen.getByTestId('who').textContent).toBe('anon'));
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    routes['POST /v1/auth/logout'] = {};
  });
});

describe('I-30 boot（F5 静默续签，I-13 bootRefresh）', () => {
  it('无 refresh_token：booting 直落 false、不发刷新', async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    renderSession();
    await waitFor(() => expect(S?.booting).toBe(false));
    expect(screen.getByTestId('who').textContent).toBe('anon');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('有 refresh_token：裸 fetch 续签成功 → 新 Bearer 拉 me → refresh 滚动覆盖', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-old');
    const fetchSpy = vi.fn(async () =>
      new Response(JSON.stringify({ access_token: 'acc-boot', refresh_token: 'ref-new', expires_in_s: 1800 }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
    vi.stubGlobal('fetch', fetchSpy);
    renderSession();
    await waitFor(() => expect(screen.getByTestId('who').textContent).toBe('alice'));
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(meAuthHeader).toBe('Bearer acc-boot');
    expect(localStorage.getItem(TOKEN_KEY)).toBe('ref-new');
  });

  it('续签失败（401）：refresh_token 被清、后续 me 请求不携带旧 Bearer、booting 落 false 不卡死', async () => {
    localStorage.setItem(TOKEN_KEY, 'expired');
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{"error":"expired"}', { status: 401 })));
    renderSession();
    await waitFor(() => expect(S?.booting).toBe(false));
    await waitFor(() => expect(localStorage.getItem(TOKEN_KEY)).toBeNull());
    expect(meAuthHeader).not.toContain('Bearer acc');
  });
});

describe('I-31 useSession 边界', () => {
  it('无 Provider 必须抛错（防静默 undefined）', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    function Sink() {
      useSession();
      return null;
    }
    expect(() => render(<Sink />)).toThrow(/useSession must be used within SessionProvider/);
    spy.mockRestore();
  });
});
