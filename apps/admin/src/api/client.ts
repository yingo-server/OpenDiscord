/**
 * 管理 API 客户端（docs/admin-data-design.md §8）。
 *
 * 统一负责四件事，业务模块不重复实现：
 *  1. Bearer access token（sessionStorage；refresh 走 httpOnly cookie）
 *  2. X-CSRF-Token 双提交校验（登录/刷新响应下发）
 *  3. 写操作 Idempotency-Key（UUID v4，5 分钟窗口内服务端去重）
 *  4. 错误体归一化 {code, reason, request_id}；401 单次静默刷新
 */

const ACCESS_TOKEN_KEY = 'oc.admin.access_token';
const CSRF_TOKEN_KEY = 'oc.admin.csrf_token';

/** 管理 API 前缀；Caddy 只对白名单 IP 反代 /api/admin/*。 */
export const API_BASE = (import.meta.env.VITE_API_BASE ?? '/api/admin/v1').replace(/\/$/, '');

export interface AdminIdentity {
  id: string;
  username: string;
  display_name: string;
  role: 'superadmin' | 'admin' | 'moderator' | 'auditor';
  totp_enabled: boolean;
}

export interface TokenBundle {
  access_token: string;
  expires_in: number;
  csrf_token: string;
  admin: AdminIdentity;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly reason: string;
  readonly requestId: string;

  constructor(status: number, code: string, reason: string, requestId: string) {
    super(`${code}: ${reason}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.reason = reason;
    this.requestId = requestId;
  }

  /** 401/403 是权限问题，网络错误 status=0 可重试。 */
  get retryable(): boolean {
    return this.status === 0 || this.status >= 500;
  }
}

/* ------------------------------------------------------------------ */
/* 会话存储                                                            */
/* ------------------------------------------------------------------ */

let accessToken: string | null = sessionStorage.getItem(ACCESS_TOKEN_KEY);
let csrfToken: string | null = sessionStorage.getItem(CSRF_TOKEN_KEY);

export function getAccessToken(): string | null {
  return accessToken;
}

export function getCsrfToken(): string | null {
  return csrfToken;
}

export function saveSession(bundle: TokenBundle): void {
  accessToken = bundle.access_token;
  csrfToken = bundle.csrf_token;
  sessionStorage.setItem(ACCESS_TOKEN_KEY, bundle.access_token);
  sessionStorage.setItem(CSRF_TOKEN_KEY, bundle.csrf_token);
  sessionStorage.setItem('oc.admin.identity', JSON.stringify(bundle.admin));
}

export function clearSession(): void {
  accessToken = null;
  csrfToken = null;
  sessionStorage.removeItem(ACCESS_TOKEN_KEY);
  sessionStorage.removeItem(CSRF_TOKEN_KEY);
  sessionStorage.removeItem('oc.admin.identity');
}

export function cachedIdentity(): AdminIdentity | null {
  const raw = sessionStorage.getItem('oc.admin.identity');
  if (!raw) return null;
  try {
    return JSON.parse(raw) as AdminIdentity;
  } catch {
    return null;
  }
}

/* ------------------------------------------------------------------ */
/* 幂等键                                                              */
/* ------------------------------------------------------------------ */

/**
 * 生成写操作幂等键。crypto.randomUUID 仅在安全上下文可用；
 * 内网 http:// 访问时退回 getRandomValues，保证格式始终是 UUID v4。
 */
export function newIdempotencyKey(): string {
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === 'function') return c.randomUUID();
  const bytes = new Uint8Array(16);
  if (c && typeof c.getRandomValues === 'function') {
    c.getRandomValues(bytes);
  } else {
    for (let i = 0; i < 16; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/* ------------------------------------------------------------------ */
/* 查询串                                                              */
/* ------------------------------------------------------------------ */

export type QueryValue = string | number | boolean | null | undefined | Array<string | number>;

export function qs(params: Record<string, QueryValue>): string {
  const sp = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;
    if (Array.isArray(value)) {
      for (const item of value) sp.append(key, String(item));
    } else {
      sp.set(key, String(value));
    }
  }
  const out = sp.toString();
  return out ? `?${out}` : '';
}

/* ------------------------------------------------------------------ */
/* 请求                                                                */
/* ------------------------------------------------------------------ */

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  body?: unknown;
  query?: Record<string, QueryValue>;
  signal?: AbortSignal;
  /** 显式幂等键（重试/重放场景复用同一键，避免二次生效） */
  idempotencyKey?: string;
  /** 内部标记：401 已重试过，避免递归 */
  _retried?: boolean;
  timeoutMs?: number;
}

const WRITE_METHODS = new Set(['POST', 'PATCH', 'PUT', 'DELETE']);

async function parseError(res: Response): Promise<ApiError> {
  let code = `HTTP_${res.status}`;
  let reason = res.statusText || 'request failed';
  let requestId = res.headers.get('X-Request-ID') ?? '';
  try {
    const text = await res.text();
    if (text) {
      try {
        const body = JSON.parse(text) as { code?: string; reason?: string; request_id?: string };
        if (body.code) code = body.code;
        if (body.reason) reason = body.reason;
        if (body.request_id) requestId = body.request_id;
      } catch {
        reason = text.slice(0, 300);
      }
    }
  } catch {
    /* body 读取失败：保留状态行信息 */
  }
  return new ApiError(res.status, code, reason, requestId);
}

async function rawRequest(path: string, opts: RequestOptions = {}): Promise<Response> {
  const method = opts.method ?? 'GET';
  const url = `${API_BASE}${path}${qs(opts.query ?? {})}`;
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'X-Client': 'ufot-admin/0.1.0',
  };
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  if (accessToken) headers.Authorization = `Bearer ${accessToken}`;
  if (csrfToken && WRITE_METHODS.has(method)) headers['X-CSRF-Token'] = csrfToken;
  if (WRITE_METHODS.has(method)) {
    headers['Idempotency-Key'] = opts.idempotencyKey ?? newIdempotencyKey();
  }

  const controller = new AbortController();
  const timeout = window.setTimeout(() => controller.abort(), opts.timeoutMs ?? 30_000);
  const onAbort = () => controller.abort();
  if (opts.signal) {
    if (opts.signal.aborted) controller.abort();
    else opts.signal.addEventListener('abort', onAbort, { once: true });
  }

  try {
    return await fetch(url, {
      method,
      headers,
      credentials: 'include',
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: controller.signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') {
      throw new ApiError(0, 'TIMEOUT', `请求超时（${path}）`, '');
    }
    throw new ApiError(0, 'NETWORK_ERROR', (err as Error).message || '网络不可达', '');
  } finally {
    window.clearTimeout(timeout);
    if (opts.signal) opts.signal.removeEventListener('abort', onAbort);
  }
}

/* 单飞刷新：并发 401 只发一次 /auth/refresh */
let refreshInFlight: Promise<boolean> | null = null;

async function refreshSession(): Promise<boolean> {
  if (!refreshInFlight) {
    refreshInFlight = (async () => {
      try {
        const res = await rawRequest('/auth/refresh', { method: 'POST', body: {}, timeoutMs: 10_000 });
        if (!res.ok) return false;
        const bundle = (await res.json()) as TokenBundle;
        saveSession(bundle);
        return true;
      } catch {
        return false;
      } finally {
        refreshInFlight = null;
      }
    })();
  }
  return refreshInFlight;
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const res = await rawRequest(path, opts);

  if (res.status === 401 && !opts._retried) {
    const ok = await refreshSession();
    if (ok) return request<T>(path, { ...opts, _retried: true });
    clearSession();
    throw new ApiError(401, 'UNAUTHENTICATED', '登录已失效，请重新登录', '');
  }

  if (!res.ok) throw await parseError(res);
  if (res.status === 204) return undefined as T;

  const text = await res.text();
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

export const api = {
  get: <T>(path: string, query?: Record<string, QueryValue>, signal?: AbortSignal) =>
    request<T>(path, { method: 'GET', query, signal }),
  post: <T>(path: string, body?: unknown, opts: Omit<RequestOptions, 'method' | 'body'> = {}) =>
    request<T>(path, { ...opts, method: 'POST', body: body ?? {} }),
  patch: <T>(path: string, body?: unknown, opts: Omit<RequestOptions, 'method' | 'body'> = {}) =>
    request<T>(path, { ...opts, method: 'PATCH', body: body ?? {} }),
  del: <T>(path: string, body?: unknown, opts: Omit<RequestOptions, 'method' | 'body'> = {}) =>
    request<T>(path, { ...opts, method: 'DELETE', body }),
};

/** 取纯文本（Prometheus /metrics 用）。 */
export async function getText(path: string, signal?: AbortSignal): Promise<string> {
  const res = await rawRequest(path, { method: 'GET', signal });
  if (!res.ok) throw await parseError(res);
  return res.text();
}

/* ------------------------------------------------------------------ */
/* 认证                                                                */
/* ------------------------------------------------------------------ */

export interface LoginChallenge {
  mfa_required: boolean;
  mfa_ticket?: string;
  /** 账密正确但尚未通过 TOTP 时，服务端可能已下发会话，故字段可选 */
  session?: TokenBundle;
}

export const auth = {
  login: (username: string, password: string) =>
    request<LoginChallenge>('/auth/login', { method: 'POST', body: { username, password } }),
  verifyTotp: (mfaTicket: string, code: string) =>
    request<TokenBundle>('/auth/login/totp', {
      method: 'POST',
      body: { mfa_ticket: mfaTicket, code },
    }),
  me: () => request<AdminIdentity>('/auth/me'),
  logout: async () => {
    try {
      await request<void>('/auth/logout', { method: 'POST', body: {} });
    } finally {
      clearSession();
    }
  },
};
