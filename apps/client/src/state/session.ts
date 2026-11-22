import type { Session } from '../domain/types';
import { getMemorySession, setMemorySession, wipeSession } from '../platform/session';
import { ApiError } from '../net/http/client';

type Listener = (session: Session | null) => void;

let session: Session | null = null;
const listeners = new Set<Listener>();

function emit(): void {
  for (const listener of listeners) listener(session);
}

export function subscribeSession(listener: Listener): () => void {
  listeners.add(listener);
  listener(session);
  return () => listeners.delete(listener);
}

export function getSession(): Session | null {
  return session;
}

export function setSession(next: Session | null): void {
  session = next;
  if (next) {
    setMemorySession({
      accessToken: next.access_token,
      refreshToken: next.refresh_token,
      userId: next.user_id,
      deviceId: next.device_id,
      guest: next.guest,
      expiresAt: next.expires_at,
    });
    if (next.refresh_token) sessionStorage.setItem('ufot.refresh', next.refresh_token);
  } else {
    wipeSession();
    clearStoredRefresh();
  }
  emit();
}

export function storedRefreshToken(): string | null {
  return sessionStorage.getItem('ufot.refresh');
}

function clearStoredRefresh(): void {
  sessionStorage.removeItem('ufot.refresh');
}

export function requireAuth(): Session {
  if (!session) throw new ApiError('UNAUTHORIZED', '请先登录', 401);
  return session;
}
