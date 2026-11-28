/**
 * 会话与用户领域层。
 *
 * 数据来源（2026-11-28 定稿）：**全部走 WS 数据通道**，不依赖 HTTP 端点。
 *   - 930 CONV_DATA_REQUEST op7  列出「我参与的会话」
 *     （服务端经 conv_members 联结，只返回调用者自己的会话）
 *   - 940 IDENTITY_REQUEST op5   按关键字搜索用户
 *   - 940 IDENTITY_REQUEST op2   读取用户资料
 *   - 940 IDENTITY_REQUEST op1   更新自己的资料
 * 回执分别是 931 CONV_DATA_RESULT / 941 IDENTITY_RESULT，载荷形状见
 * core/net/ws/kinds.ts 与 server/internal/data/pipe/wire/wire.go。
 *
 * HTTP 面只有认证/媒体/推送三个域（见 docs/client-contract.md §8），会话与
 * 用户检索从来没有 HTTP 端点——早期草案 frontend/docs/backend-endpoint-proposal.md
 * 里的 HTTP 方案已作废，改为上面的 WS 通道实现。
 */

import type { Conversation, UserProfile } from './types';

/** 930 CONV_DATA 的 op 号（与 server/internal/data/pipe/conv.go 一致）。 */
export const ConvOp = {
  CREATE: 1,
  ADD_MEMBER: 2,
  REMOVE_MEMBER: 3,
  SET_ROLE: 4,
  GET_META: 5,
  LIST_MEMBERS: 6,
  LIST_MINE: 7,
} as const;

/** 940 IDENTITY 的 op 号（与 server/internal/data/domain/identity 一致）。 */
export const IdentityOp = {
  UPSERT_USER: 1,
  GET_USER: 2,
  GET_BAN: 3,
  SET_BAN: 4,
  SEARCH_USER: 5,
} as const;

/** 会话类型（与后端 conv.go 的常量逐字一致）：1=单聊 2=群聊。 */
export const ConvType = {
  DIRECT: 1,
  GROUP: 2,
} as const;

/** 930 op7 请求载荷（空对象即可，actor 取自会话身份）。 */
export function listMinePayload(): Record<string, unknown> {
  return {};
}

/** 940 op5 请求载荷。 */
export function searchUsersPayload(query: string): Record<string, unknown> {
  return { query };
}

/** 940 op2 请求载荷：读取指定用户资料。 */
export function getUserPayload(userId: number): Record<string, unknown> {
  return { userId };
}

/** 940 op1 请求载荷：更新自己的资料。 */
export function upsertUserPayload(displayName: string): Record<string, unknown> {
  return { displayName };
}

/** 931/941 的 DataResult 形状（wire.DataResult）。 */
export interface DataResult {
  op?: number;
  ok?: boolean;
  error?: string;
  /** base64 的业务 JSON（wire 里 []byte 的 JSON 编码）。 */
  body?: string;
  requestId?: string;
}

/** 930 op7 的 body：会话列表。 */
export interface ConvListBody {
  items?: Conversation[];
}

/** 940 op5 的 body：用户列表。 */
export interface UserListBody {
  items?: UserProfile[];
}

/** 940 op2 的 body：单个用户资料。 */
export interface UserBody {
  user_id?: number;
  display_name?: string;
  avatar_media_id?: string;
  status?: number;
  created_at?: number;
}

export interface CreateConversationInput {
  title?: string;
  /** 1=单聊 2=群聊（见 ConvType） */
  convType?: number;
  /** 单聊：对方 user_id；群聊：初始成员 */
  memberIds?: number[];
}
