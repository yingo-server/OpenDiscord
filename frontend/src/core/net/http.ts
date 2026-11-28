/**
 * HTTP 客户端：统一错误归一 + 鉴权传递。
 *
 * 后端事实：
 * - 错误体两种形态并存：{error, code} 与 {code, message}，都要兼容。
 * - 登录成功下发 HttpOnly Cookie（SameSite=Lax）。跨源部署时浏览器不会在
 *   XHR 上带该 Cookie，因此客户端同时使用 Authorization: Bearer <token>。
 * - 429 带 retry_after_ms（毫秒）。
 */

export interface ApiErrorShape {
  code: string;
  message: string;
  status: number;
  retryAfterMs?: number;
}

export class ApiError extends Error implements ApiErrorShape {
  readonly code: string;
  readonly status: number;
  readonly retryAfterMs?: number;

  constructor(shape: ApiErrorShape) {
    super(shape.message);
    this.name = 'ApiError';
    this.code = shape.code;
    this.status = shape.status;
    this.retryAfterMs = shape.retryAfterMs;
  }

  /** 令牌失效类错误：应回到登录态。 */
  get isAuthError(): boolean {
    return (
      this.status === 401 ||
      this.code === 'UNAUTHORIZED' ||
      this.code === 'UNAUTHENTICATED' ||
      this.code === 'INVALID_TOKEN' ||
      this.code === 'REFRESH_REPLAY' ||
      this.code === 'REFRESH_REVOKED' ||
      this.code === 'REFRESH_EXPIRED' ||
      this.code === 'INVALID_REFRESH'
    );
  }
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
  body?: unknown;
  /** 附加请求头 */
  headers?: Record<string, string>;
  /** 是否携带鉴权（默认 true） */
  auth?: boolean;
  /** 覆盖基址（管理面用） */
  base?: string;
  /** 是否带 Cookie（默认 true；跨源部署可关） */
  withCredentials?: boolean;
  signal?: AbortSignal;
  /** 期望的响应类型 */
  expect?: 'json' | 'text' | 'blob' | 'none';
}

type TokenProvider = () => string | null;

let tokenProvider: TokenProvider = () => null;

/** 注册令牌来源（access token）。core/store/auth 会调用。 */
export function setAuthTokenProvider(fn: TokenProvider): void {
  tokenProvider = fn;
}

function buildUrl(base: string, path: string): string {
  if (/^https?:\/\//i.test(path)) return path;
  return `${base}${path.startsWith('/') ? path : `/${path}`}`;
}

/** 从后端五花八门的错误体里抽出 {code, message, retry_after_ms}。 */
export function normalizeErrorBody(body: unknown, status: number): ApiErrorShape {
  const fallback = `请求失败（HTTP ${status}）`;
  if (!body || typeof body !== 'object') {
    return { code: `HTTP_${status}`, message: fallback, status };
  }
  const rec = body as Record<string, unknown>;
  const code =
    typeof rec.code === 'string'
      ? rec.code
      : typeof rec.reason === 'string'
        ? rec.reason
        : `HTTP_${status}`;
  const rawMessage =
    (typeof rec.error === 'string' && rec.error) ||
    (typeof rec.message === 'string' && rec.message) ||
    (typeof rec.reason === 'string' && rec.reason) ||
    fallback;
  const retryAfterMs =
    typeof rec.retry_after_ms === 'number'
      ? rec.retry_after_ms
      : typeof rec.retryAfterMs === 'number'
        ? rec.retryAfterMs
        : undefined;
  return { code, message: rawMessage, status, retryAfterMs };
}

async function parseBody(response: Response, expect: RequestOptions['expect']): Promise<unknown> {
  if (expect === 'none' || response.status === 204) return null;
  const contentType = response.headers.get('content-type') || '';
  if (expect === 'blob') return response.blob();
  if (expect === 'text') return response.text();
  if (contentType.includes('application/json')) {
    try {
      return await response.json();
    } catch {
      return null;
    }
  }
  const text = await response.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

import { appendDebug, redactHeaders, summarizeBody } from '@/core/debug/db';

/** 发起一次 API 请求；失败一律抛 ApiError（带 code / status / retryAfterMs）。 */
export async function request<T = unknown>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const started = Date.now();
  const {
    method = 'GET',
    body,
    headers = {},
    auth = true,
    base = '',
    withCredentials = true,
    signal,
    expect = 'json',
  } = options;

  const finalHeaders: Record<string, string> = { Accept: 'application/json', ...headers };
  let payload: BodyInit | undefined;
  if (body instanceof FormData) {
    payload = body;
  } else if (body !== undefined) {
    finalHeaders['Content-Type'] = finalHeaders['Content-Type'] ?? 'application/json';
    payload = JSON.stringify(body);
  }

  if (auth) {
    const token = tokenProvider();
    if (token) finalHeaders.Authorization = `Bearer ${token}`;
  }

  const url = buildUrl(base, path);
  let response: Response;
  try {
    response = await fetch(url, {
      method,
      headers: finalHeaders,
      body: payload,
      credentials: withCredentials ? 'include' : 'omit',
      signal,
    });
    void appendDebug({
      kind: 'http', direction: 'out', method, url, status: response.status,
      headers: redactHeaders(finalHeaders), body: summarizeBody(body), durationMs: Date.now() - started,
    });
  } catch (cause) {
    void appendDebug({
      kind: 'error', direction: 'out', method, url,
      headers: redactHeaders(finalHeaders), body: summarizeBody(body),
      durationMs: Date.now() - started,
      error: cause instanceof Error ? cause.message : String(cause),
    });
    throw new ApiError({
      code: 'NETWORK_ERROR',
      message: cause instanceof Error ? cause.message : '网络请求失败',
      status: 0,
    });
  }

  const parsed = await parseBody(response, expect);
  void appendDebug({
    kind: 'http', direction: 'in', method, url, status: response.status,
    body: summarizeBody(parsed), durationMs: Date.now() - started, ok: response.ok,
  });
  if (!response.ok) {
    throw new ApiError(normalizeErrorBody(parsed, response.status));
  }
  return parsed as T;
}

/** 拼接查询参数（跳过 undefined / null / 空串）。 */
export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const usp = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;
    usp.set(key, String(value));
  }
  const text = usp.toString();
  return text ? `?${text}` : '';
}
