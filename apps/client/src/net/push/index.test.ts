import { describe, expect, it, vi } from 'vitest';
import {
  PUSH_SUBSCRIBE_ENDPOINT,
  ensureNotificationPermission,
  pushEnabled,
  registerServiceWorker,
  subscribeAndReport,
  vapidPublicKey,
} from './index';
import { decide } from './sw-strategy';

// ---------------------------------------------------------------------------
// SW 缓存策略判定（sw.js 内联逻辑的镜像）
// ---------------------------------------------------------------------------

describe('sw-strategy decide', () => {
  it('bypasses /api/ and /ws entirely', () => {
    expect(decide('/api/media/1', 'GET', true)).toBe('bypass');
    expect(decide('/api/push/subscribe', 'POST', true)).toBe('bypass');
    expect(decide('/ws', 'GET', true)).toBe('bypass');
    expect(decide('/api/media/1', 'GET', false)).toBe('bypass');
  });

  it('treats same-origin static shell as shell', () => {
    expect(decide('/', 'GET', true)).toBe('shell');
    expect(decide('/index.html', 'GET', true)).toBe('shell');
    expect(decide('/manifest.webmanifest', 'GET', true)).toBe('shell');
    expect(decide('/assets/index-abc.js', 'GET', true)).toBe('shell');
    expect(decide('/icons/icon-192.png', 'GET', true)).toBe('shell');
  });

  it('never caches cross-origin or non-GET shell-ish paths', () => {
    expect(decide('/index.html', 'GET', false)).toBe('pass');
    expect(decide('/assets/index-abc.js', 'POST', true)).toBe('pass');
  });

  it('bypasses GET /api/media/ like all /api/ paths (never cached)', () => {
    expect(decide('/api/media/abc', 'GET', true)).toBe('bypass');
  });

  it('passes everything else through', () => {
    expect(decide('/some/random/page', 'GET', true)).toBe('pass');
  });
});

// ---------------------------------------------------------------------------
// pushEnabled：production only，dev/mock 不注册
// ---------------------------------------------------------------------------

describe('pushEnabled', () => {
  it('disabled when DEV', () => {
    expect(pushEnabled({ DEV: true, VITE_MOCK: '' })).toBe(false);
  });

  it('disabled when mock mode', () => {
    expect(pushEnabled({ DEV: false, VITE_MOCK: '1' })).toBe(false);
  });

  it('enabled in production non-mock', () => {
    expect(pushEnabled({ DEV: false, VITE_MOCK: '' })).toBe(true);
  });
});

describe('registerServiceWorker dev skip', () => {
  it('does not register in dev/mock mode', async () => {
    const register = vi.fn();
    expect(await registerServiceWorker(register, { DEV: true, VITE_MOCK: '' })).toBeNull();
    expect(register).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// fail-closed：VAPID 未配置 / 无 SW / 服务端未实现
// ---------------------------------------------------------------------------

describe('vapidPublicKey', () => {
  it('returns empty string when not configured', () => {
    expect(vapidPublicKey()).toBe('');
  });
});

describe('subscribeAndReport fail-closed', () => {
  it('returns null without subscribing when VAPID key missing', async () => {
    const post = vi.fn();
    const result = await subscribeAndReport({ vapidKey: '', post });
    expect(result).toBeNull();
    expect(post).not.toHaveBeenCalled();
  });

  it('returns null when no registration available', async () => {
    const subscribe = vi.fn();
    const result = await subscribeAndReport({ vapidKey: 'pub-key', registration: null, subscribe });
    expect(result).toBeNull();
    expect(subscribe).not.toHaveBeenCalled();
  });

  it('reports to server endpoint on success', async () => {
    const subscribe = vi.fn().mockResolvedValue({ endpoint: 'https://push.example/x', toJSON: () => ({ endpoint: 'https://push.example/x' }) });
    const post = vi.fn().mockResolvedValue(true);
    const result = await subscribeAndReport({ vapidKey: 'pub-key', registration: {} as never, subscribe, post });
    expect(result?.toJSON()).toEqual({ endpoint: 'https://push.example/x' });
    expect(post).toHaveBeenCalledWith(PUSH_SUBSCRIBE_ENDPOINT, {
      subscription: { endpoint: 'https://push.example/x' },
    });
  });

  it('server not implemented (non-2xx) → no throw, returns subscription', async () => {
    const subscribe = vi.fn().mockResolvedValue({ endpoint: 'e', toJSON: () => ({ endpoint: 'e' }) });
    const post = vi.fn().mockResolvedValue(false);
    const result = await subscribeAndReport({ vapidKey: 'pub-key', registration: {} as never, subscribe, post });
    expect(result?.toJSON()).toEqual({ endpoint: 'e' });
  });

  it('pushManager.subscribe rejection → null, no post', async () => {
    const subscribe = vi.fn().mockRejectedValue(new Error('denied'));
    const post = vi.fn();
    const result = await subscribeAndReport({ vapidKey: 'pub-key', registration: {} as never, subscribe, post });
    expect(result).toBeNull();
    expect(post).not.toHaveBeenCalled();
  });
});

describe('ensureNotificationPermission fail-closed', () => {
  it('false when permission denied', async () => {
    vi.stubGlobal('Notification', { permission: 'denied', requestPermission: vi.fn() });
    expect(await ensureNotificationPermission()).toBe(false);
  });

  it('false when Notification API missing', async () => {
    vi.stubGlobal('Notification', undefined);
    expect(await ensureNotificationPermission()).toBe(false);
  });
});
