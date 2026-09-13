// 会话上下文（14号 §2）：login→POST /v1/auth/login；登出→POST /v1/auth/logout；
// 当前用户→GET /v1/users/me（网关注入 access_token）
// V2.1 (ADR-205)：register→POST /v1/auth/register（注册即登录，返回令牌对）；
// CurrentUser 带 role（菜单/路由门禁）与 must_change_password（首登强改密）。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { api, bootRefresh, clearSession, getAccessToken, readRefreshToken, saveRefreshToken, setAccessToken } from '../api/client';
import { queryClient } from '../api/queryClient';

export interface CurrentUser {
  user_id: string;
  username: string;
  email: string;
  role?: string; // proto Role 枚举字符串（ROLE_ADMIN 等）；旧令牌/旧缓存可能缺省
  must_change_password?: boolean;
  state?: string;
  created_at?: string | null;
}

interface SessionCtx {
  user: CurrentUser | null;
  booting: boolean;
  login: (username: string, password: string) => Promise<void>;
  register: (username: string, email: string, password: string, inviteCode: string) => Promise<void>;
  logout: () => Promise<void>;
  refreshUser: () => Promise<void>;
}

const Ctx = createContext<SessionCtx | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<CurrentUser | null>(null);
  const [booting, setBooting] = useState(true);

  // F5/直链恢复会话：有 refresh_token 则静默续签（否则直接进登录页）。
  // P-22：续签成功=会话有效，me 失败可能只是瞬时限流/网关抖动（GUI 429 风暴实证：
  // 旧口径首败即 user=null，活跃用户被静默甩到登录页）——退避重试后才认未登录。
  useEffect(() => {
    if (!readRefreshToken()) {
      setBooting(false);
      return;
    }
    let cancelled = false;
    bootRefresh()
      .then(async () => {
        for (const delayMs of [1000, 2000, 4000]) {
          try {
            await refreshUserRef.current();
            return;
          } catch {
            if (!cancelled) await new Promise((r) => setTimeout(r, delayMs));
          }
        }
        try {
          await refreshUserRef.current(); // 重试耗尽后末次尝试，仍败则保持未登录口径
        } catch {
          /* user=null → 登录页（与真实无凭据一致的兜底） */
        }
      })
      .catch(() => {})
      .finally(() => {
        if (!cancelled) setBooting(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const refreshUser = useCallback(async () => {
    const resp = await api.get<CurrentUser>('/v1/users/me');
    setUser(resp.data);
  }, []);

  const refreshUserRef = useRef(refreshUser);
  refreshUserRef.current = refreshUser;

  const login = useCallback(async (username: string, password: string) => {
    const resp = await api.post('/v1/auth/login', { username, password });
    const { access_token, refresh_token } = resp.data;
    setAccessToken(access_token);
    // Q3 裁决：refresh_token 存 localStorage（XSS 下可被读取，风险显式接受）。
    // 纵深缓解（修正口径，原注释"严格 CSP"名不副实——此前无任何响应头落地）：
    // nginx 已下发最小安全响应头（nosniff / frame-ancestors 'none' / object-src 'none'，
    // 见 nginx/default.conf.template）；CSP 未覆盖脚本/样式源（SPA 内联依赖），收紧待真机验证。
    saveRefreshToken(refresh_token);
    await refreshUser();
  }, [refreshUser]);

  // V2.1 (ADR-205)：注册即登录——RegisterUser 响应即令牌对
  const register = useCallback(async (username: string, email: string, password: string, inviteCode: string) => {
    const resp = await api.post('/v1/auth/register', {
      username,
      email,
      password,
      invite_code: inviteCode || undefined,
    });
    const { access_token, refresh_token } = resp.data;
    setAccessToken(access_token);
    saveRefreshToken(refresh_token);
    await refreshUser();
  }, [refreshUser]);

  const logout = useCallback(async () => {
    try {
      // proto L1203: 需携带 access_token（此前空 body 恒 400，前端清会话掩盖了错误）
      await api.post('/v1/auth/logout', { access_token: getAccessToken() });
    } finally {
      clearSession();
      setUser(null);
      // B5-P2-6: 清 TanStack 缓存——软登出（SPA 跳 /login）后跨账号数据不残留
      queryClient.clear();
    }
  }, [getAccessToken]);

  const value = useMemo(
    () => ({ user, booting, login, register, logout, refreshUser }),
    [user, booting, login, register, logout, refreshUser],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): SessionCtx {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error('useSession must be used within SessionProvider');
  return ctx;
}
