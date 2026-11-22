/** Web Push 订阅：VAPID 公钥缺省时不启用、不报错。 */
import { env } from '../../platform/env';
import { HttpClient, ApiError } from '../http/client';

function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const normalized = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(normalized);
  const output = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) output[i] = raw.charCodeAt(i);
  return output;
}

export function pushAvailable(): boolean {
  return Boolean(env.vapidPublicKey && 'serviceWorker' in navigator && 'PushManager' in window);
}

export async function subscribePush(http: HttpClient): Promise<void> {
  if (!pushAvailable()) throw new ApiError('PUSH_UNAVAILABLE', '当前环境不支持推送', 0);
  const registration = await navigator.serviceWorker.register('/sw.js');
  await navigator.serviceWorker.ready;
  const existing = await registration.pushManager.getSubscription();
  const subscription = existing
    || await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(env.vapidPublicKey),
    });
  const json = subscription.toJSON() as { endpoint?: string; keys?: { p256dh?: string; auth?: string } };
  await http.post('/api/push/subscribe', {
    endpoint: json.endpoint,
    keys: { p256dh: json.keys?.p256dh, auth: json.keys?.auth },
  });
}
