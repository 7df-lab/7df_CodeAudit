// 会话上下文（14号 §2）：login→POST /v1/auth/login；登出→POST /v1/auth/logout；
// 当前用户→GET /v1/users/me（网关注入 access_token）
// V2.1 (ADR-205)：register→POST /v1/auth/register（注册即登录，返回令牌对）；
// CurrentUser 带 role（菜单/路由门禁）与 must_change_password（首登强改密）。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { api, bootRefresh, clearSession, getAccessToken, readRefreshToken, saveRefreshToken, setAccessToken } from '../api/client';

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

  // F5/直链恢复会话：有 refresh_token 则静默续签（否则直接进登录页）
  useEffect(() => {
    if (!readRefreshToken()) {
      setBooting(false);
      return;
    }
    bootRefresh()
      .then(() => refreshUserRef.current())
      .catch(() => {})
      .finally(() => setBooting(false));
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
    saveRefreshToken(refresh_token); // Q3 裁决：localStorage + 严格 CSP，风险显式接受
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
