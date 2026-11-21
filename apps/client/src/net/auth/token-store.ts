/**
 * TokenStore：会话令牌与 device_id 的唯一持有者。
 *
 * - 内存优先，localStorage 持久化（隐私模式自动降级为纯内存）；
 * - device_id 首次生成后稳定不变（多设备踢下线 / RESUME 需要）；
 * - 不校验 token 内容（无状态 JWT 由服务端验签），只负责生命周期。
 */
import type { AuthSession } from './provider';

export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface StoredSession {
  token: string;
  refreshToken?: string;
  userId: number;
  sessionId: string;
  expiresAt: number;
  guest: boolean;
  displayName?: string;
}

const SESSION_KEY = 'ufot.session';
const DEVICE_KEY = 'ufot.device';

function safeStorage(): StorageLike | null {
  try {
    if (typeof localStorage === 'undefined') return null;
    const probe = '__ufot_probe__';
    localStorage.setItem(probe, '1');
    localStorage.removeItem(probe);
    return localStorage;
  } catch {
    return null;
  }
}

export class TokenStore {
  private session: StoredSession | null = null;
  private deviceId: string;
  private listeners = new Set<(session: StoredSession | null) => void>();
  private readonly storage: StorageLike | null;

  constructor(storage: StorageLike | null = safeStorage()) {
    this.storage = storage;
    this.session = this.readSession();
    this.deviceId = this.readDeviceId();
  }

  get current(): StoredSession | null {
    if (!this.session) return null;
    if (this.session.expiresAt > 0 && this.session.expiresAt < Date.now()) {
      return null; // 过期即视为未登录；refresh 由上层重新登录完成
    }
    return this.session;
  }

  get accessToken(): string | null {
    return this.current?.token ?? null;
  }

  get userId(): number {
    return this.current?.userId ?? 0;
  }

  get sessionId(): string {
    return this.current?.sessionId ?? '';
  }

  get guest(): boolean {
    return this.current?.guest ?? false;
  }

  getDeviceId(): string {
    return this.deviceId;
  }

  set(session: AuthSession): StoredSession {
    const stored: StoredSession = {
      token: session.token,
      refreshToken: session.refreshToken,
      userId: session.userId,
      sessionId: session.sessionId,
      expiresAt: session.expiresAt,
      guest: session.guest ?? false,
      displayName: session.profile?.displayName,
    };
    this.session = stored;
    this.persistSession();
    this.listeners.forEach((fn) => fn(stored));
    return stored;
  }

  clear(): void {
    this.session = null;
    try {
      this.storage?.removeItem(SESSION_KEY);
    } catch {
      /* ignore */
    }
    this.listeners.forEach((fn) => fn(null));
  }

  subscribe(fn: (session: StoredSession | null) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private readSession(): StoredSession | null {
    try {
      const raw = this.storage?.getItem(SESSION_KEY);
      if (!raw) return null;
      const parsed = JSON.parse(raw) as StoredSession;
      if (!parsed || typeof parsed.token !== 'string') return null;
      return parsed;
    } catch {
      return null;
    }
  }

  private readDeviceId(): string {
    try {
      const existing = this.storage?.getItem(DEVICE_KEY);
      if (existing) return existing;
      const id = generateId();
      this.storage?.setItem(DEVICE_KEY, id);
      return id;
    } catch {
      return generateId();
    }
  }

  private persistSession(): void {
    try {
      if (this.session) this.storage?.setItem(SESSION_KEY, JSON.stringify(this.session));
      else this.storage?.removeItem(SESSION_KEY);
    } catch {
      /* 配额满不影响当前会话 */
    }
  }
}

function generateId(): string {
  const cryptoObj = typeof crypto !== 'undefined' ? crypto : undefined;
  if (cryptoObj && typeof cryptoObj.randomUUID === 'function') return cryptoObj.randomUUID();
  return `dev_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}
