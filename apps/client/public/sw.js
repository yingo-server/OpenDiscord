/**
 * OpenDiscord Service Worker（PWA 壳 + Web Push）。
 *
 * 策略（与 src/net/push/sw-strategy.ts 镜像，二者需人工保持一致）：
 * - /api/ 与 /ws 一律 bypass，绝不缓存；
 * - 静态壳（同源 GET：html/css/js/字体/图标/manifest）cache-first，miss 时缓存；
 * - 已缓存媒体（/api/media/ 命中缓存时）走 stale 回包；
 * - 导航请求失败时离线回退 index.html。
 *
 * 注意：本文件在构建时原样复制到站点根，不能 import src/ 下的 TS 模块，
 * 因此判定逻辑在此内联一份并在 sw-strategy.ts 留镜像 + 单测。
 */
const SHELL_CACHE = 'od-shell-v1';
const MEDIA_CACHE = 'od-media-v1';

/** /api/ 与 /ws 永不缓存、永不拦截（含 /api/media 上行，按 GET 区分见下）。 */
function shouldBypassCache(url) {
  return url.pathname.startsWith('/api/') || url.pathname.startsWith('/ws');
}

/** 静态壳：同源 GET 的 html/css/js/字体/图标/manifest。 */
function isShellAsset(url, request) {
  if (request.method !== 'GET' || url.origin !== self.location.origin) return false;
  if (url.pathname === '/' || url.pathname === '/index.html') return true;
  if (url.pathname === '/manifest.webmanifest') return true;
  if (url.pathname.startsWith('/icons/')) return true;
  return (
    url.pathname.startsWith('/assets/') ||
    /\.(css|js|mjs|woff2?|ttf|otf|png|svg|ico)$/.test(url.pathname)
  );
}

/** 已缓存媒体可离线回放：仅 GET /api/media/{id} 且缓存命中时回缓存。 */
function isMediaAsset(url, request) {
  return request.method === 'GET' && url.pathname.startsWith('/api/media/');
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches
      .open(SHELL_CACHE)
      .then((cache) => cache.addAll(['/index.html', '/manifest.webmanifest']))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((k) => k !== SHELL_CACHE && k !== MEDIA_CACHE)
            .map((k) => caches.delete(k)),
        ),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (shouldBypassCache(url)) return; // 浏览器原生处理 /api/ 与 /ws

  if (isShellAsset(url, event.request)) {
    event.respondWith(
      caches.match(event.request).then(
        (hit) =>
          hit ||
          fetch(event.request)
            .then((resp) => {
              if (resp.ok) {
                const copy = resp.clone();
                caches.open(SHELL_CACHE).then((c) => c.put(event.request, copy));
              }
              return resp;
            })
            .catch(() => caches.match('/index.html')),
      ),
    );
    return;
  }

  if (isMediaAsset(url, event.request)) {
    event.respondWith(
      caches.match(event.request).then((hit) => {
        if (hit) return hit;
        return fetch(event.request).then((resp) => {
          if (resp.ok) {
            const copy = resp.clone();
            caches.open(MEDIA_CACHE).then((c) => c.put(event.request, copy));
          }
          return resp;
        });
      }),
    );
  }
});

// ---------------------------------------------------------------------------
// Web Push：push 事件 → 系统通知；点击 → focus 既有会话窗口。
// ---------------------------------------------------------------------------

/** 与服务端 payload 契约：{ title?, body?, convId? }；缺失时走 i18n 兜底文案的英文常量镜像。 */
self.addEventListener('push', (event) => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch (e) {
    payload = { body: event.data ? event.data.text() : '' };
  }
  const title = payload.title || 'OpenDiscord';
  const body = payload.body || '';
  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag: payload.convId ? `conv-${payload.convId}` : undefined,
      data: { convId: payload.convId ?? null },
    }),
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const convId = event.notification.data && event.notification.data.convId;
  const target = convId ? `/?conv=${encodeURIComponent(String(convId))}` : '/';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        if (client.url.includes(self.location.origin)) {
          if (convId) client.postMessage({ type: 'push-focus-conv', convId });
          return client.focus();
        }
      }
      return self.clients.openWindow(target);
    }),
  );
});
