export interface EnvConfig {
  apiBase: string;
  wsUrl: string;
  defaultLang: string;
  devWsUrl: string;
}

interface ViteEnv {
  VITE_API_BASE?: string;
  VITE_WS_URL?: string;
  VITE_DEFAULT_LANG?: string;
  VITE_DEV_WS_URL?: string;
  DEV?: boolean;
}

function httpUrl(input: string): URL | null {
  try {
    const url = new URL(input, 'http://placeholder.local');
    if (url.protocol === 'http:' || url.protocol === 'https:') return url;
    return null;
  } catch {
    return null;
  }
}

function toWs(base: URL): string {
  const proto = base.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${base.host}/ws`;
}

function origin(location: Pick<Location, 'origin' | 'protocol' | 'host'>): string {
  if (location.origin && location.origin !== 'null') return location.origin;
  return `${location.protocol}//${location.host}`;
}

export function resolveApiBase(
  raw: string | undefined,
  location: Pick<Location, 'origin' | 'protocol' | 'host'> | undefined,
): string {
  const configured = (raw || '').trim();
  if (configured) {
    const trimmed = configured.replace(/\/+$/, '');
    return httpUrl(trimmed) ? trimmed : origin(location || { origin: '', protocol: 'http:', host: 'localhost' });
  }
  if (!location) return '';
  return origin(location);
}

export function resolveWsUrl(
  env: ViteEnv,
  location: Pick<Location, 'origin' | 'protocol' | 'host'> | undefined,
): string {
  const explicit = (env.VITE_WS_URL || '').trim();
  if (explicit) return explicit;

  const loc = location || { origin: '', protocol: 'http:', host: 'localhost' };
  if (env.DEV && env.VITE_DEV_WS_URL) return env.VITE_DEV_WS_URL;

  const api = resolveApiBase(env.VITE_API_BASE, loc);
  if (api) {
    const url = httpUrl(api);
    if (url) return toWs(url);
  }
  return `${loc.protocol === 'https:' ? 'wss' : 'ws'}://${loc.host}/ws`;
}

export function loadEnv(raw: ViteEnv, location?: Location): EnvConfig {
  return {
    apiBase: resolveApiBase(raw.VITE_API_BASE, location),
    wsUrl: resolveWsUrl(raw, location),
    defaultLang: (raw.VITE_DEFAULT_LANG || 'zh-CN').trim() || 'zh-CN',
    devWsUrl: (raw.VITE_DEV_WS_URL || '').trim(),
  };
}

export const env: EnvConfig = loadEnv(import.meta.env as ViteEnv, typeof location === 'undefined' ? undefined : location);
