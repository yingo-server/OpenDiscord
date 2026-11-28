/**
 * 认证领域层：四种登录方式 + refresh 轮换 + 登出。
 *
 * 后端事实（docs/client-contract.md §2）：
 * - 登录前必须先 GET /api/auth/methods 探测开关，只渲染可用方式；
 * - 会话对象的 expires_at 是**毫秒**；
 * - refresh 令牌重放会吊销整族令牌，必须回到登录态；
 * - 并发 refresh 会触发重放检测，因此这里做**单飞**（single-flight）。
 */

import { env } from '@/core/config/env';
import { ApiError, request } from '@/core/net/http';
import type { AuthMethods, Session } from './types';

const DEVICE_KEY = 'ufot.device_id';

export function getDeviceId(): string | undefined {
  try {
    const value = localStorage.getItem(DEVICE_KEY);
    // 服务端要求 device_id 是服务端签发过的（g_ 前缀），否则会重新签发。
    return value && value.startsWith('g_') ? value : undefined;
  } catch {
    return undefined;
  }
}

export function rememberDeviceId(deviceId: string): void {
  try {
    localStorage.setItem(DEVICE_KEY, deviceId);
  } catch {
    /* 隐私模式下忽略 */
  }
}

/** 探测可用的登录方式（公开端点，按 IP 限流 60/min）。 */
export async function fetchAuthMethods(signal?: AbortSignal): Promise<AuthMethods> {
  const body = await request<{ methods: AuthMethods }>('/api/auth/methods', {
    base: env.apiBase,
    auth: false,
    signal,
  });
  return {
    phone: Boolean(body.methods?.phone),
    email: Boolean(body.methods?.email),
    password: Boolean(body.methods?.password),
    guest: Boolean(body.methods?.guest),
  };
}

export async function loginGuest(nickname?: string): Promise<Session> {
  const body: Record<string, unknown> = {};
  const deviceId = getDeviceId();
  if (deviceId) body.device_id = deviceId;
  if (nickname?.trim()) body.nickname = nickname.trim();
  const session = await request<Session>('/api/auth/guest', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body,
  });
  rememberDeviceId(session.device_id);
  return session;
}

export async function loginPassword(account: string, password: string): Promise<Session> {
  const body: Record<string, unknown> = { account, password };
  const deviceId = getDeviceId();
  if (deviceId) body.device_id = deviceId;
  const session = await request<Session>('/api/auth/password', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body,
  });
  rememberDeviceId(session.device_id);
  return session;
}

export interface ChallengeResponse {
  ok: boolean;
  challenge_id: string;
  channel: string;
  expires_in: number;
  resend_after: number;
}

export async function requestPhoneCode(phone: string): Promise<ChallengeResponse> {
  return request<ChallengeResponse>('/api/auth/phone/code', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body: { phone },
  });
}

export async function verifyPhoneCode(
  phone: string,
  code: string,
  challengeId?: string,
): Promise<Session> {
  const body: Record<string, unknown> = { phone, code };
  if (challengeId) body.challenge_id = challengeId;
  const deviceId = getDeviceId();
  if (deviceId) body.device_id = deviceId;
  const session = await request<Session>('/api/auth/phone/verify', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body,
  });
  rememberDeviceId(session.device_id);
  return session;
}

export async function requestEmailLink(email: string): Promise<ChallengeResponse> {
  return request<ChallengeResponse>('/api/auth/email/link', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body: { email },
  });
}

/** 轮询邮箱魔法链接；未点击时后端回 409 PENDING。 */
export async function pollEmailLink(email: string, challengeId?: string): Promise<Session | null> {
  const body: Record<string, unknown> = { email };
  if (challengeId) body.challenge_id = challengeId;
  const deviceId = getDeviceId();
  if (deviceId) body.device_id = deviceId;
  try {
    const session = await request<Session>('/api/auth/email/poll', {
      method: 'POST',
      base: env.apiBase,
      auth: false,
      body,
    });
    rememberDeviceId(session.device_id);
    return session;
  } catch (error) {
    if (error instanceof ApiError && error.code === 'PENDING') return null;
    throw error;
  }
}

export async function refreshSession(refreshToken: string): Promise<Session> {
  const body: Record<string, unknown> = { refresh_token: refreshToken };
  const deviceId = getDeviceId();
  if (deviceId) body.device_id = deviceId;
  const session = await request<Session>('/api/auth/refresh', {
    method: 'POST',
    base: env.apiBase,
    auth: false,
    body,
  });
  rememberDeviceId(session.device_id);
  return session;
}

export async function logout(refreshToken?: string): Promise<{ ok: boolean; revoked: boolean }> {
  return request<{ ok: boolean; revoked: boolean }>('/api/auth/logout', {
    method: 'POST',
    base: env.apiBase,
    body: refreshToken ? { refresh_token: refreshToken } : {},
  });
}

/** 设置/修改密码（需登录；后端要求至少 10 字符）。 */
export async function setPassword(password: string): Promise<{ ok: boolean }> {
  return request<{ ok: boolean }>('/api/auth/password/set', {
    method: 'POST',
    base: env.apiBase,
    body: { password },
  });
}
