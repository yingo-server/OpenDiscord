const COOKIE_NAME = 'access_token';

export function readCookie(name: string): string | null {
  if (typeof document === 'undefined') return null;
  const found = document.cookie.split(';').map((part) => part.trim()).find((part) => part.startsWith(`${name}=`));
  return found ? decodeURIComponent(found.slice(name.length + 1)) : null;
}

export function clearCookie(name: string): void {
  document.cookie = `${name}=; Path=/; Max-Age=0; SameSite=Lax`;
}

export interface MemorySession {
  accessToken: string;
  refreshToken: string;
  userId: number;
  deviceId: string;
  guest: boolean;
  expiresAt: number;
}

let memorySession: MemorySession | null = null;

export function setMemorySession(session: MemorySession | null): void {
  memorySession = session;
}

export function getMemorySession(): MemorySession | null {
  return memorySession;
}

export function isSessionFresh(session: MemorySession | null): boolean {
  if (!session || !session.accessToken) return false;
  const expiry = session.expiresAt;
  if (!Number.isFinite(expiry) || expiry <= 0) return true;
  return expiry > Date.now() + (expiry > 1e12 ? 30_000 : 30);
}

export function getAccessToken(): string | null {
  if (isSessionFresh(memorySession)) return memorySession!.accessToken;
  return readCookie(COOKIE_NAME);
}

export function wipeSession(): void {
  memorySession = null;
  clearCookie(COOKIE_NAME);
}

export function deviceId(): string {
  let id = sessionStorage.getItem('ufot.device');
  if (!id) {
    id = `web_${Math.random().toString(36).slice(2) + Date.now().toString(36)}`;
    sessionStorage.setItem('ufot.device', id);
  }
  return id;
}
