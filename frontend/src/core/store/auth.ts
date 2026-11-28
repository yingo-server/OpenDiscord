/**
 * 认证状态：会话持久化、令牌轮换（单飞）、登出。
 */

import { create } from 'zustand';
import { ApiError, setAuthTokenProvider } from '@/core/net/http';
import * as authApi from '@/core/domain/auth';
import type { AuthMethods, Session } from '@/core/domain/types';

const SESSION_KEY = 'ufot.session';

type AuthStatus = 'unknown' | 'anonymous' | 'authenticated';

interface AuthState {
  status: AuthStatus;
  session: Session | null;
  methods: AuthMethods | null;
  methodsError: string | null;
  busy: boolean;
  error: string | null;
  notice: string | null;

  loadMethods: (signal?: AbortSignal) => Promise<void>;
  restore: () => Promise<void>;
  login: (input: LoginInput) => Promise<void>;
  logout: () => Promise<void>;
  setPassword: (password: string) => Promise<void>;
  updateProfile: (patch: { display_name?: string }) => void;
  clearError: () => void;
  setNotice: (notice: string | null) => void;
}

export type LoginInput =
  | { method: 'guest'; nickname?: string }
  | { method: 'password'; account: string; password: string }
  | { method: 'phone'; phone: string; code: string; challengeId?: string }
  | { method: 'email'; email: string; challengeId?: string };

function loadPersisted(): Session | null {
  try {
    const raw = localStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Session;
    if (!parsed?.access_token || !parsed?.refresh_token) return null;
    return parsed;
  } catch {
    return null;
  }
}

function persist(session: Session | null): void {
  try {
    if (session) localStorage.setItem(SESSION_KEY, JSON.stringify(session));
    else localStorage.removeItem(SESSION_KEY);
  } catch {
    /* 忽略存储失败 */
  }
}

/** 令牌轮换单飞：并发 refresh 会触发后端重放检测并吊销整族令牌。 */
let refreshInFlight: Promise<Session | null> | null = null;

async function refreshOnce(session: Session): Promise<Session | null> {
  if (!refreshInFlight) {
    refreshInFlight = authApi
      .refreshSession(session.refresh_token)
      .then((next) => {
        persist(next);
        return next;
      })
      .catch((error) => {
        if (error instanceof ApiError && error.isAuthError) return null;
        throw error;
      })
      .finally(() => {
        refreshInFlight = null;
      });
  }
  return refreshInFlight;
}

export const useAuthStore = create<AuthState>((set, get) => ({
  status: 'unknown',
  session: null,
  methods: null,
  methodsError: null,
  busy: false,
  error: null,
  notice: null,

  async loadMethods(signal) {
    try {
      const methods = await authApi.fetchAuthMethods(signal);
      set({ methods, methodsError: null });
    } catch (error) {
      const message = error instanceof Error ? error.message : '无法获取登录方式';
      set({ methodsError: message });
    }
  },

  async restore() {
    const persisted = loadPersisted();
    if (!persisted) {
      set({ status: 'anonymous', session: null });
      return;
    }
    set({ status: 'authenticated', session: persisted });
    // access token 过期（按毫秒判断）就静默续期；失败则回登录态。
    const expired = Date.now() > persisted.expires_at - 30_000;
    if (!expired) return;
    try {
      const next = await refreshOnce(persisted);
      if (!next) {
        persist(null);
        set({ status: 'anonymous', session: null });
      } else {
        set({ session: next });
      }
    } catch {
      /* 续期失败不阻塞 UI，用户手动重登即可 */
    }
  },

  async login(input) {
    set({ busy: true, error: null, notice: null });
    try {
      let session: Session;
      switch (input.method) {
        case 'guest':
          session = await authApi.loginGuest(input.nickname);
          break;
        case 'password':
          session = await authApi.loginPassword(input.account, input.password);
          break;
        case 'phone':
          session = await authApi.verifyPhoneCode(input.phone, input.code, input.challengeId);
          break;
        case 'email': {
          const sessionOrNull = await authApi.pollEmailLink(input.email, input.challengeId);
          if (!sessionOrNull) {
            set({ busy: false, notice: '邮件链接尚未被点击，请稍候或重新发送。' });
            return;
          }
          session = sessionOrNull;
          break;
        }
      }
      persist(session);
      set({ session, status: 'authenticated', busy: false });
    } catch (error) {
      const message = error instanceof Error ? error.message : '登录失败';
      set({ error: message, busy: false });
    }
  },

  async logout() {
    const session = get().session;
    try {
      if (session) await authApi.logout(session.refresh_token);
    } catch {
      /* 登出失败不阻塞本地清理 */
    }
    persist(null);
    set({ session: null, status: 'anonymous' });
  },

  async setPassword(password) {
    set({ busy: true, error: null });
    try {
      await authApi.setPassword(password);
      set({ busy: false, notice: '密码已更新。' });
    } catch (error) {
      const message = error instanceof Error ? error.message : '修改密码失败';
      set({ error: message, busy: false });
      throw error;
    }
  },

  updateProfile(patch) {
    const session = get().session;
    if (!session) return;
    const next: Session = {
      ...session,
      profile: { ...(session.profile ?? {}), ...patch },
    };
    persist(next);
    set({ session: next });
  },

  clearError() {
    set({ error: null });
  },

  setNotice(notice) {
    set({ notice });
  },
}));

// HTTP 层通过这里取 Bearer 令牌（跨源部署时 Cookie 不会被浏览器带上）。
setAuthTokenProvider(() => useAuthStore.getState().session?.access_token ?? null);
