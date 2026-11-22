import { describe, expect, it } from 'vitest';
import { loadEnv, resolveApiBase, resolveWsUrl } from './env';

const loc = { origin: 'https://chat.example.com', protocol: 'https:', host: 'chat.example.com' };

describe('env resolution', () => {
  it('uses same-origin defaults', () => {
    expect(resolveApiBase(undefined, loc)).toBe('https://chat.example.com');
    expect(resolveWsUrl({}, loc as Location)).toBe('wss://chat.example.com/ws');
  });

  it('derives WS from VITE_API_BASE', () => {
    const env = loadEnv({ VITE_API_BASE: 'https://server.344977.xyz' }, loc as Location);
    expect(env.apiBase).toBe('https://server.344977.xyz');
    expect(env.wsUrl).toBe('wss://server.344977.xyz/ws');
    expect(env.defaultLang).toBe('zh-CN');
  });

  it('prefers explicit VITE_WS_URL', () => {
    expect(resolveWsUrl({ VITE_WS_URL: 'wss://relay.example.com/custom' }, loc as Location)).toBe('wss://relay.example.com/custom');
  });

  it('uses local WS only in dev', () => {
    expect(resolveWsUrl({ DEV: true, VITE_DEV_WS_URL: 'ws://127.0.0.1:8080/ws' }, loc as Location)).toBe('ws://127.0.0.1:8080/ws');
    expect(resolveWsUrl({ VITE_DEV_WS_URL: 'ws://127.0.0.1:8080/ws' }, loc as Location)).toBe('wss://chat.example.com/ws');
  });

  it('falls back to a relative path base', () => {
    expect(resolveApiBase('', undefined)).toBe('');
    expect(resolveWsUrl({}, undefined)).toBe('ws://localhost/ws');
  });
});
