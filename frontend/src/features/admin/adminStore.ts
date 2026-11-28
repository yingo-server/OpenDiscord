/** 管理面会话状态：token + CSRF 双提交令牌（与用户端登录完全隔离）。 */

import { create } from 'zustand';
import * as adminApi from '@/core/domain/admin';
import type { AdminIdentity } from '@/core/domain/admin';

const STORAGE_KEY = 'ufot.admin';

interface AdminState {
  token: string | null;
  csrfToken: string | null;
  identity: AdminIdentity | null;
  busy: boolean;
  error: string | null;

  login: (username: string, password: string, totp?: string) => Promise<void>;
  logout: () => Promise<void>;
  whoami: () => Promise<void>;
  context: () => adminApi.AdminContext;
}

function persist(state: { token: string | null; csrfToken: string | null }): void {
  try {
    if (state.token && state.csrfToken) {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } else {
      localStorage.removeItem(STORAGE_KEY);
    }
  } catch {
    /* 忽略 */
  }
}

function restore(): { token: string | null; csrfToken: string | null } {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { token: null, csrfToken: null };
    const parsed = JSON.parse(raw) as { token?: string; csrfToken?: string };
    return { token: parsed.token ?? null, csrfToken: parsed.csrfToken ?? null };
  } catch {
    return { token: null, csrfToken: null };
  }
}

const initial = restore();

export const useAdminStore = create<AdminState>((set, get) => ({
  token: initial.token,
  csrfToken: initial.csrfToken,
  identity: null,
  busy: false,
  error: null,

  async login(username, password, totp) {
    set({ busy: true, error: null });
    try {
      const result = await adminApi.adminLogin(username, password, totp);
      persist({ token: result.token, csrfToken: result.csrf_token });
      set({
        token: result.token,
        csrfToken: result.csrf_token,
        identity: {
          admin_id: result.admin.admin_id,
          username: result.admin.username,
          role: result.admin.role,
          expires_at: result.expires_at,
        },
        busy: false,
      });
    } catch (error) {
      set({ error: error instanceof Error ? error.message : '管理员登录失败', busy: false });
    }
  },

  async logout() {
    const ctx = get().context();
    try {
      await adminApi.adminLogout(ctx);
    } catch {
      /* 忽略 */
    }
    persist({ token: null, csrfToken: null });
    set({ token: null, csrfToken: null, identity: null });
  },

  async whoami() {
    try {
      const identity = await adminApi.adminWhoami(get().context());
      set({ identity });
    } catch (error) {
      set({ error: error instanceof Error ? error.message : '无法获取管理员信息' });
    }
  },

  context() {
    return { token: get().token, csrfToken: get().csrfToken };
  },
}));
