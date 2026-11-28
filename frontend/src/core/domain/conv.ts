/**
 * 会话与用户领域层。
 *
 * 现状（2026-11-28 实测 + 源码核对）：后端用户面**没有**列出我的会话 /
 * 搜索用户的 HTTP 端点，客户端只能靠 WS 事件（kind 202 CONV_PAYLOAD）累积。
 * 这里按约定的补充端点实现（草案见 frontend/docs/backend-endpoint-proposal.md）：
 *   GET /api/conversations            -> {items:[Conversation], total}
 *   GET /api/conversations/{id}       -> Conversation
 *   GET /api/users?q=&limit=          -> {items:[UserProfile], total}
 * 若后端尚未提供（404），调用方走 fallback：事件累积 + 本地缓存。
 */

import { env } from '@/core/config/env';
import { ApiError, qs, request } from '@/core/net/http';
import type { Conversation, Paged, UserProfile } from './types';

export interface ConversationListQuery {
  limit?: number;
  offset?: number;
  /** 关键字（标题 / 成员昵称） */
  q?: string;
}

export async function listConversations(
  query: ConversationListQuery = {},
  signal?: AbortSignal,
): Promise<Paged<Conversation>> {
  const body = await request<Paged<Conversation>>(
    `/api/conversations${qs({
      limit: query.limit ?? 50,
      offset: query.offset ?? 0,
      q: query.q,
    })}`,
    { base: env.apiBase, signal },
  );
  return { items: body.items ?? [], total: body.total ?? 0 };
}

export async function getConversation(convId: number): Promise<Conversation> {
  return request<Conversation>(`/api/conversations/${convId}`, { base: env.apiBase });
}

export interface UserSearchQuery {
  q?: string;
  limit?: number;
  offset?: number;
}

export async function searchUsers(
  query: UserSearchQuery = {},
  signal?: AbortSignal,
): Promise<Paged<UserProfile>> {
  const body = await request<Paged<UserProfile>>(
    `/api/users${qs({ q: query.q, limit: query.limit ?? 30, offset: query.offset ?? 0 })}`,
    { base: env.apiBase, signal },
  );
  return { items: body.items ?? [], total: body.total ?? 0 };
}

/** 端点是否已上线：404 视为“未实现”，调用方走降级路径而不是报错。 */
export function isEndpointMissing(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 404 || error.code === 'NOT_FOUND');
}

/** 会话创建（走 WS kind 200 的载荷形状，此处仅描述入参）。 */
export interface CreateConversationInput {
  title?: string;
  /** 0=单聊 1=群聊（与服务端 convType 一致） */
  convType?: number;
  memberIds?: number[];
}
