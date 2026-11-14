/**
 * Web Push（VAPID）客户端封装。
 *
 * 契约（服务端接口缝，可能尚未实现——见 lts-1.4-plan §5 第 6 行）：
 *   POST /api/push/subscribe  Cookie 鉴权，body { subscription: PushSubscriptionJSON }
 *     → 204 订阅已登记
 *     → 401/404/500 任何非 2xx：fail-closed，仅记日志，不弹错、不重试风暴。
 *
 * 服务端未实现 / 浏览器不支持 / 未配置 VAPID 时一律 fail-closed：
 * 不抛错、不上报 toast，只 console.info，让主流程（WS 长连接 + 页内通知）继续。
 */
import { t } from '../../ui/i18n';

/** 服务端接口缝（§2 规定路径）。 */
export const PUSH_SUBSCRIBE_ENDPOINT = '/api/push/subscribe';

/** VAPID 公钥来源：构建期注入，缺省视为未配置。 */
export function vapidPublicKey(): string {
  const key = import.meta.env?.VITE_VAPID_PUBLIC_KEY as string | undefined;
  return typeof key === 'string' && key.length > 0 ? key : '';
}

/** 当前环境是否允许注册 SW / 订阅推送：production only，dev/mock 不注册。 */
export function pushEnabled(): boolean {
  const isMock = import.meta.env?.VITE_MOCK === '1';
  const isDev = Boolean(import.meta.env?.DEV);
  return !isDev && !isMock;
}

/**
 * 注册 SW（pushEnabled 为 false 时返回 null）。sw.js 与 src 同源。
 * @param register 注入以便单测；默认 navigator.serviceWorker.register。
 */
export async function registerServiceWorker(
  register: (script: string) => Promise<ServiceWorker> = (script) =>
    navigator.serviceWorker.register(script),
): Promise<ServiceWorker | null> {
  if (!pushEnabled()) {
    console.info('[push] disabled (dev/mock mode), skip service worker registration');
    return null;
  }
  if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) return null;
  try {
    return await register('/sw.js');
  } catch (e) {
    console.info('[push] service worker registration failed', e);
    return null;
  }
}

/**
 * 申请通知权限。拒绝 / 忽略返回 false（fail-closed，不重试）。
 */
export async function ensureNotificationPermission(): Promise<boolean> {
  if (typeof Notification === 'undefined') return false;
  if (Notification.permission === 'granted') return true;
  if (Notification.permission === 'denied') return false;
  try {
    return (await Notification.requestPermission()) === 'granted';
  } catch {
    return false;
  }
}

/**
 * VAPID 订阅并 POST 到服务端接口缝。任何一步不可用都 fail-closed 返回 null。
 * @param deps 注入以便单测（默认走真实浏览器 API 与 fetch）。
 */
export async function subscribeAndReport(deps: {
  vapidKey?: string;
  registration?: ServiceWorkerRegistration | null;
  subscribe?: (options: { userVisibleOnly: boolean; applicationServerKey: string }) => Promise<PushSubscription>;
  post?: (url: string, body: unknown) => Promise<boolean>;
} = {}): Promise<PushSubscription | null> {
  const vapidKey = deps.vapidKey ?? vapidPublicKey();
  if (!vapidKey) {
    console.info('[push] VAPID public key not configured, fail-closed');
    return null;
  }
  const registration = deps.registration ?? (await registerServiceWorker());
  if (!registration) {
    console.info('[push] no service worker, fail-closed');
    return null;
  }
  const subscribe =
    deps.subscribe ??
    ((options) => registration.pushManager.subscribe(options) as Promise<PushSubscription>);
  let subscription: PushSubscription;
  try {
    subscription = await subscribe({ userVisibleOnly: true, applicationServerKey: vapidKey });
  } catch (e) {
    console.info('[push] pushManager.subscribe failed', e);
    return null;
  }
  const post =
    deps.post ??
    (async (url: string, body: unknown) => {
      try {
        const resp = await fetch(url, {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        return resp.ok;
      } catch {
        return false;
      }
    });
  const ok = await post(PUSH_SUBSCRIBE_ENDPOINT, { subscription: subscription.toJSON() });
  if (!ok) {
    // 服务端接口缝尚未实现：只记日志，不弹错（fail-closed）。
    console.info(t('push.subscribeFailed'));
    return subscription; // 订阅本身有效，仅登记失败；下次启动可重试登记
  }
  return subscription;
}
