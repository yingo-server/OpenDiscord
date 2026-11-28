/**
 * 管理面（/api/v1/*）。
 *
 * 后端事实（docs/api-reference.md §5）：
 * - 独立端口 admin_rest_listen（默认回环 7781），生产需由反向代理暴露到 /api/v1；
 * - 鉴权：Authorization: Bearer <token> 或 Cookie oc_admin_session；
 * - **所有非安全方法都要带 CSRF 双提交**（X-CSRF-Token == Cookie oc_admin_csrf）；
 * - 写方法建议带 Idempotency-Key（5 分钟窗口逐字节重放）；
 * - 错误体统一 {code:int, reason:string, request_id:string}。
 */

import { env } from '@/core/config/env';
import { ApiError, qs, request } from '@/core/net/http';

export interface AdminIdentity {
  admin_id: number;
  username: string;
  role: number;
  permissions?: string[];
  expires_at?: number;
}

export interface AdminLoginResponse {
  token: string;
  csrf_token: string;
  expires_at: number;
  admin: { admin_id: number; username: string; role: number };
}

export interface AdminListResponse<T> {
  items: T[];
  total: number;
}

export interface AdminUserRecord {
  id?: number;
  user_id?: number;
  username?: string;
  display_name?: string;
  status?: number;
  created_at?: number;
  [key: string]: unknown;
}

export interface AdminReportRecord {
  id?: number;
  status?: number;
  reason?: string;
  created_at?: number;
  [key: string]: unknown;
}

export interface AdminMessageRecord {
  id?: number;
  msg_id?: number;
  conv_id?: number;
  sender_id?: number;
  text?: string;
  created_at?: number;
  [key: string]: unknown;
}

export interface AdminFlagRecord {
  key: string;
  value: string;
  version: number;
}

export interface AdminAuditRecord {
  id?: number;
  action?: string;
  actor?: string;
  at?: number;
  [key: string]: unknown;
}

const ADMIN_BASE = () => env.adminBase;

/** 管理面请求上下文：Bearer token + CSRF + 幂等键。 */
export interface AdminContext {
  token: string | null;
  csrfToken: string | null;
}

function adminHeaders(ctx: AdminContext, method: string, extra?: Record<string, string>) {
  const headers: Record<string, string> = { ...extra };
  if (ctx.token) headers.Authorization = `Bearer ${ctx.token}`;
  const safe = method === 'GET' || method === 'HEAD';
  if (!safe && ctx.csrfToken) headers['X-CSRF-Token'] = ctx.csrfToken;
  if (!safe) headers['Idempotency-Key'] = crypto.randomUUID();
  return headers;
}

async function adminRequest<T>(
  ctx: AdminContext,
  path: string,
  options: {
    method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
    body?: unknown;
    query?: Record<string, string | number | boolean | undefined>;
    expect?: 'json' | 'text' | 'none';
  } = {},
): Promise<T> {
  const method = options.method ?? 'GET';
  const fullUrl = options.query ? `${ADMIN_BASE()}${path}${qs(options.query)}` : `${ADMIN_BASE()}${path}`;
  try {
    return await request<T>(fullUrl, {
      method,
      base: '',
      body: options.body,
      headers: adminHeaders(ctx, method),
      expect: options.expect,
      // 管理面错误体是 {code:int, reason, request_id}，由 http.ts 统一归一。
      auth: false,
    });
  } catch (error) {
    throw error;
  }
}

export async function adminLogin(
  username: string,
  password: string,
  totp?: string,
): Promise<AdminLoginResponse> {
  const body: Record<string, unknown> = { username, password };
  if (totp) body.totp = totp;
  const result = await request<AdminLoginResponse>('/api/v1/login', {
    method: 'POST',
    base: ADMIN_BASE(),
    body,
    auth: false,
  });
  return result;
}

export async function adminLogout(ctx: AdminContext): Promise<{ ok: boolean }> {
  return adminRequest<{ ok: boolean }>(ctx, '/api/v1/logout', { method: 'POST' });
}

export async function adminWhoami(ctx: AdminContext): Promise<AdminIdentity> {
  return adminRequest<AdminIdentity>(ctx, '/api/v1/whoami');
}

export async function adminListUsers(
  ctx: AdminContext,
  query: { status?: number; limit?: number; offset?: number; q?: string } = {},
): Promise<AdminListResponse<AdminUserRecord>> {
  return adminRequest<AdminListResponse<AdminUserRecord>>(ctx, '/api/v1/users', { query });
}

export async function adminGetUser(
  ctx: AdminContext,
  id: number,
): Promise<AdminUserRecord> {
  return adminRequest<AdminUserRecord>(ctx, `/api/v1/users/${id}`);
}

export async function adminBanUser(
  ctx: AdminContext,
  id: number,
  input: { level?: number; reason?: string; days?: number; until?: number },
): Promise<unknown> {
  return adminRequest(ctx, `/api/v1/users/${id}/ban`, { method: 'POST', body: input });
}

export async function adminUnbanUser(
  ctx: AdminContext,
  id: number,
  reason?: string,
): Promise<unknown> {
  return adminRequest(ctx, `/api/v1/users/${id}/unban`, {
    method: 'POST',
    body: reason ? { reason } : {},
  });
}

export async function adminKickUser(
  ctx: AdminContext,
  id: number,
  reason?: string,
): Promise<unknown> {
  return adminRequest(ctx, `/api/v1/users/${id}/kick`, {
    method: 'POST',
    body: reason ? { reason } : {},
  });
}

export async function adminListReports(
  ctx: AdminContext,
  query: { status?: number; limit?: number; offset?: number } = {},
): Promise<AdminListResponse<AdminReportRecord>> {
  return adminRequest<AdminListResponse<AdminReportRecord>>(ctx, '/api/v1/reports', { query });
}

export async function adminGetReport(
  ctx: AdminContext,
  id: number,
): Promise<AdminReportRecord> {
  return adminRequest<AdminReportRecord>(ctx, `/api/v1/reports/${id}`);
}

export async function adminHandleReport(
  ctx: AdminContext,
  id: number,
  action: 'claim' | 'resolve' | 'reject',
  note?: string,
): Promise<AdminReportRecord> {
  return adminRequest<AdminReportRecord>(ctx, `/api/v1/reports/${id}/handle`, {
    method: 'POST',
    body: { action, note },
  });
}

export async function adminSearchMessages(
  ctx: AdminContext,
  query: { q?: string; conv_id?: number; limit?: number; offset?: number } = {},
): Promise<AdminListResponse<AdminMessageRecord>> {
  return adminRequest<AdminListResponse<AdminMessageRecord>>(ctx, '/api/v1/messages/search', {
    query,
  });
}

export async function adminRecallMessage(
  ctx: AdminContext,
  id: number,
  reason?: string,
): Promise<unknown> {
  return adminRequest(ctx, `/api/v1/messages/${id}/recall`, {
    method: 'POST',
    body: reason ? { reason } : {},
  });
}

export async function adminListFlags(ctx: AdminContext): Promise<{ items: AdminFlagRecord[] }> {
  const body = await adminRequest<{ items: AdminFlagRecord[] }>(ctx, '/api/v1/flags');
  return { items: body.items ?? [] };
}

export async function adminSetFlags(
  ctx: AdminContext,
  input: { key?: string; value?: string; flags?: Record<string, string> },
): Promise<{ items: AdminFlagRecord[] }> {
  const body = await adminRequest<{ items: AdminFlagRecord[] }>(ctx, '/api/v1/flags', {
    method: 'PUT',
    body: input,
  });
  return { items: body.items ?? [] };
}

export async function adminAudit(
  ctx: AdminContext,
  query: { since_id?: number; limit?: number } = {},
): Promise<{ items: AdminAuditRecord[]; next_since_id?: number; has_more?: boolean }> {
  return adminRequest(ctx, '/api/v1/audit', { query });
}

export async function adminBroadcast(
  ctx: AdminContext,
  input: { text: string; conv_ids?: number[]; expires_at?: number },
  dryRun = false,
): Promise<unknown> {
  return adminRequest(ctx, '/api/v1/broadcast', {
    method: 'POST',
    body: input,
    query: dryRun ? { dry_run: true } : undefined,
  });
}

export async function adminArchiveStatus(ctx: AdminContext): Promise<unknown> {
  return adminRequest(ctx, '/api/v1/archive/status');
}

export async function adminRunArchive(
  ctx: AdminContext,
  dryRun = true,
): Promise<unknown> {
  return adminRequest(ctx, '/api/v1/archive/run', {
    method: 'POST',
    body: { dry_run: dryRun },
  });
}

export async function adminMetrics(ctx: AdminContext): Promise<string> {
  return adminRequest<string>(ctx, '/api/v1/metrics', { expect: 'text' });
}

export async function adminTurnCheck(ctx: AdminContext): Promise<unknown> {
  return adminRequest(ctx, '/api/v1/diagnostics/turn');
}

/** 管理面错误体解析：{code:int, reason:string, request_id:string}。 */
export function describeAdminError(error: unknown): string {
  if (error instanceof ApiError) return `${error.message}（${error.code}）`;
  return '未知错误';
}
