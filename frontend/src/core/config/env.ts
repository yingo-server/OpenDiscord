/**
 * 环境解析：后端地址只来自 env（构建期注入），代码里不硬编码任何域名。
 *
 * 解析公式严格对齐 docs/client-contract.md §1：
 *   API_BASE = env.API_BASE || location.origin
 *   WS_URL   = env.WS_URL || scheme(API_BASE) + '://' + host(API_BASE) + '/ws'
 */

export interface AppEnv {
  /** HTTP API 根，例如 http://chat.example.com */
  apiBase: string;
  /** WebSocket 全 URL */
  wsUrl: string;
  /** Web Push VAPID 公钥（Base64URL），空串表示未配置 */
  vapidPublicKey: string;
}

export interface RawEnv {
  VITE_API_BASE?: string;
  VITE_WS_URL?: string;
  VITE_VAPID_PUBLIC_KEY?: string;
}

/** 去掉尾部斜杠，避免拼出 `//api`。 */
export function normalizeBase(url: string): string {
  return url.replace(/\/+$/, '');
}

/** 由 API 基址推导 WS 地址；http→ws，https→wss。 */
export function deriveWsUrl(apiBase: string, origin: string): string {
  if (!apiBase) {
    const scheme = origin.startsWith('https:') ? 'wss' : 'ws';
    return `${scheme}://${origin.replace(/^https?:\/\//, '')}/ws`;
  }
  const scheme = apiBase.startsWith('https:') ? 'wss' : 'ws';
  const host = apiBase.replace(/^https?:\/\//, '').replace(/\/.*$/, '');
  return `${scheme}://${host}/ws`;
}

export function resolveEnv(raw: RawEnv, origin: string): AppEnv {
  const apiBase = normalizeBase(raw.VITE_API_BASE || origin);
  const wsUrl = raw.VITE_WS_URL ? normalizeBase(raw.VITE_WS_URL) : deriveWsUrl(apiBase, origin);
  return {
    apiBase,
    wsUrl,
    vapidPublicKey: raw.VITE_VAPID_PUBLIC_KEY || '',
  };
}

/** 页面为 https 而 WS 是 ws:// 时浏览器会按混合内容拦截，这里给出可读诊断。 */
export function wsSchemeWarning(env: AppEnv, pageProtocol: string): string | null {
  const pageIsHttps = pageProtocol === 'https:';
  const wsIsSecure = env.wsUrl.startsWith('wss:');
  if (pageIsHttps && !wsIsSecure) {
    return '当前页面是 HTTPS，但 VITE_WS_URL 是明文 ws://，浏览器会拦截 WebSocket。请把 VITE_WS_URL 改为 wss://。';
  }
  if (!pageIsHttps && wsIsSecure) {
    return '当前页面是 HTTP，但 VITE_WS_URL 是 wss://，多数浏览器会拒绝。请把 VITE_WS_URL 改为 ws://。';
  }
  return null;
}

export const env: AppEnv = resolveEnv(
  {
    VITE_API_BASE: import.meta.env.VITE_API_BASE,
    VITE_WS_URL: import.meta.env.VITE_WS_URL,
    VITE_VAPID_PUBLIC_KEY: import.meta.env.VITE_VAPID_PUBLIC_KEY,
  },
  typeof location === 'undefined' ? 'http://localhost' : location.origin,
);
