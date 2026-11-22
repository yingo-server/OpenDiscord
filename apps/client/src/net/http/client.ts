export class ApiError extends Error {
  code: string;
  status: number;

  constructor(code: string, message: string, status = 0) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
  }
}

const SAFE: Record<string, string> = {
  INVALID_CREDENTIALS: '账号或密码不正确',
  OTP_INVALID: '验证码不正确或已过期',
  BAD_REQUEST: '请检查输入内容',
  RATE_LIMITED: '操作太频繁，请稍后再试',
  UNAUTHORIZED: '请先登录',
  FORBIDDEN: '没有权限执行此操作',
  NOT_FOUND: '内容不存在',
  TOO_LARGE: '文件过大',
  NETWORK: '网络连接失败，请检查网络',
  TIMEOUT: '请求超时，请重试',
  BAD_RESPONSE: '操作未完成，请稍后重试',
  UPLOAD_FAILED: '上传未完成，请重试',
};
const FALLBACK = '操作未完成，请稍后重试';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

export function friendlyMessage(code: string): string {
  return SAFE[code] || FALLBACK;
}

export class HttpClient {
  constructor(private readonly base: string) {}

  get endpoint(): string { return this.base; }

  private url(path: string): string {
    return `${this.base.replace(/\/+$/, '')}${path.startsWith('/') ? path : `/${path}`}`;
  }

  async request<T>(path: string, init: RequestInit = {}, timeout = 15_000): Promise<T> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout);
    try {
      const res = await fetch(this.url(path), {
        ...init,
        credentials: 'include',
        signal: controller.signal,
        headers: {
          ...(init.body instanceof FormData ? {} : { 'Content-Type': 'application/json' }),
          ...(init.headers || {}),
        },
      });
      const text = await res.text();
      let value: unknown = null;
      if (text) {
        try { value = JSON.parse(text); } catch { value = null; }
      }
      if (!res.ok) {
        const code = isRecord(value) && typeof value.code === 'string' ? value.code : `HTTP_${res.status}`;
        if (res.status === 401) window.dispatchEvent(new CustomEvent('ufot:unauthorized'));
        throw new ApiError(code, friendlyMessage(code), res.status);
      }
      return value as T;
    } catch (error) {
      if (error instanceof ApiError) throw error;
      if (error instanceof DOMException && error.name === 'AbortError') throw new ApiError('TIMEOUT', friendlyMessage('TIMEOUT'));
      throw new ApiError('NETWORK', friendlyMessage('NETWORK'));
    } finally {
      clearTimeout(timer);
    }
  }

  post<T>(path: string, body: unknown = {}): Promise<T> {
    return this.request<T>(path, { method: 'POST', body: JSON.stringify(body) });
  }
}
