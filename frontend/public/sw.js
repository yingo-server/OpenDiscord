/* Web Push 接收端：只显示通用文案（服务端推送体不含消息正文）。 */

self.addEventListener('install', (event) => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('push', (event) => {
  let title = 'UFOT';
  let body = '你有新消息';
  try {
    const data = event.data ? event.data.json() : {};
    if (data && typeof data === 'object') {
      if (typeof data.title === 'string') title = data.title;
      if (typeof data.body === 'string') body = data.body;
    }
  } catch (err) {
    /* 载荷不可解析时用默认文案 */
  }
  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      icon: '/favicon.svg',
      badge: '/favicon.svg',
      tag: 'ufot-notification',
    }),
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      const target = clients.find((client) => 'focus' in client);
      if (target) return target.focus();
      return self.clients.openWindow('/');
    }),
  );
});
