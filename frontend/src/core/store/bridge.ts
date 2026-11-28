/**
 * WS 桥接层：把连接层与各领域 store 接起来。
 *
 * 职责边界：
 * - 连接层（core/net/ws）只管收发与重连；
 * - 本模块负责帧 → store 的分派，以及需要主动上行的动作（发送、输入中、回执、补拉）。
 *
 * 请求-回执配对：930/940 这类「请求-应答」帧用 request_id 配对。客户端发
 * 出的请求登记在 pending 里，服务端的 931/941 回来时按 request_id 取走并
 * resolve——这样上层（store/UI）能 await 一个真实的成功/失败，而不是盲发。
 */

import { create } from 'zustand';
import { env } from '@/core/config/env';
import { WsClient } from '@/core/net/ws/client';
import type { ConnectionState } from '@/core/net/ws/client';
import { decodePayload } from '@/core/net/ws/codec';
import { Kind } from '@/core/net/ws/kinds';
import { useConvStore } from './conv';
import { useMsgStore } from './msg';
import { useAuthStore } from './auth';
import { useUiStore } from './ui';
import { ConvType } from '@/core/domain/conv';
import type { ChatMessage, MediaItem } from '@/core/domain/types';

interface ConnState {
  state: ConnectionState;
  detail: string | null;
  latencyMs: number | null;
  kickedReason: string | null;
  setState: (state: ConnectionState, detail?: string) => void;
}

export const useConnStore = create<ConnState>((set) => ({
  state: 'idle',
  detail: null,
  latencyMs: null,
  kickedReason: null,
  setState: (state, detail) => set({ state, detail: detail ?? null }),
}));

let client: WsClient | null = null;
let typingSentAt = 0;
let typingActive = false;

/** 请求-回执配对表：request_id → resolve/reject。 */
interface PendingCall {
  resolve: (body: unknown) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}
const pending = new Map<string, PendingCall>();
let requestSeq = 0;

function newRequestId(kind: string): string {
  requestSeq += 1;
  return `${kind}-${Date.now().toString(36)}-${requestSeq}`;
}

/**
 * 发起一次「请求-应答」型调用：发出帧后等待对应回执。
 * 超时 8s 后 reject，避免 UI 永远等下去。
 */
export function requestWithReply(
  kind: number,
  payload: unknown,
  options: { convId?: number; userId?: number; timeoutMs?: number } = {},
): Promise<{ ok: boolean; error?: string; body?: unknown }> {
  return new Promise((resolve, reject) => {
    if (!client || client.state !== 'open') {
      reject(new Error('连接未就绪'));
      return;
    }
    const requestId = newRequestId(`k${kind}`);
    const timer = setTimeout(() => {
      pending.delete(requestId);
      reject(new Error('请求超时'));
    }, options.timeoutMs ?? 8000);
    pending.set(requestId, {
      resolve: (body) => resolve({ ok: true, body }),
      reject,
      timer,
    });
    const ok = client.send(kind, payload, { ...options, requestId });
    if (!ok) {
      clearTimeout(timer);
      pending.delete(requestId);
      reject(new Error('发送失败'));
    }
  });
}

function settlePending(requestId: string, ok: boolean, error: string | undefined, body: unknown) {
  const call = pending.get(requestId);
  if (!call) return false;
  clearTimeout(call.timer);
  pending.delete(requestId);
  if (ok) call.resolve(body);
  else call.reject(new Error(error ?? '请求失败'));
  return true;
}

function newClient(token: string | null): WsClient {
  return new WsClient({
    url: env.wsUrl,
    token,
    onState: (state, detail) => useConnStore.getState().setState(state, detail),
    onFrame: handleFrame,
  });
}

/** 建立连接（登出/换 token 后需重新调用）。 */
export function connectWs(token: string | null): void {
  disconnectWs();
  client = newClient(token);
  client.connect();
}

export function disconnectWs(): void {
  client?.close();
  client = null;
}

export function sendFrame(
  kind: number,
  payload?: unknown,
  options: { convId?: number; userId?: number; requestId?: string } = {},
): boolean {
  return client?.send(kind, payload, options) ?? false;
}

/** 发送一条消息（乐观发送由调用方处理）。 */
export function sendMessage(input: {
  convId: number;
  text?: string;
  media?: MediaItem[];
  replyTo?: number;
  mentions?: number[];
  clientMsgId: string;
}): boolean {
  return sendFrame(
    Kind.MSG_SEND,
    {
      clientMsgId: input.clientMsgId,
      text: input.text ?? '',
      media: input.media ?? [],
      replyTo: input.replyTo ?? 0,
      mentions: input.mentions ?? [],
    },
    { convId: input.convId },
  );
}

/** 输入中指示（服务端桶为 1/s，这里自行节流）。 */
export function sendTyping(convId: number, typing: boolean): void {
  const now = Date.now();
  if (!typing) {
    typingActive = false;
    sendFrame(Kind.TYPING, { conv_id: convId, typing: false });
    return;
  }
  if (typingActive && now - typingSentAt < 1000) return;
  typingSentAt = now;
  typingActive = true;
  sendFrame(Kind.TYPING, { conv_id: convId, typing: true });
}

/** 已读 / 送达回执。 */
export function sendReceipt(convId: number, msgId: number, rtype: 0 | 1): void {
  sendFrame(
    rtype === 0 ? Kind.MSG_READ : Kind.MSG_DELIVERED,
    { msg_id: msgId, rtype },
    { convId },
  );
}

/** 补拉缺口：sinceSeq 之后的消息会以逐条 102 返回。 */
export function requestSync(convId: number, sinceSeq: number, limit = 200): void {
  sendFrame(Kind.SYNC_REQUEST, { convId, sinceSeq, limit }, { convId });
}

/** 撤回 / 删除。 */
export function sendMutation(convId: number, msgId: number, forEveryone: boolean): void {
  sendFrame(
    forEveryone ? Kind.MSG_RECALL : Kind.MSG_DELETE,
    { msg_id: msgId, conv_id: convId, for_everyone: forEveryone },
    { convId },
  );
}

/** 编辑消息。 */
export function sendEdit(convId: number, msgId: number, text: string): void {
  sendFrame(Kind.MSG_EDIT, { msg_id: msgId, conv_id: convId, text }, { convId });
}

/** 转发消息。 */
export function sendForward(fromConvId: number, toConvId: number, msgId: number): void {
  sendFrame(
    Kind.MSG_FORWARD,
    {
      msg_id: msgId,
      from_conv_id: fromConvId,
      to_conv_id: toConvId,
      forward_from: { msg_id: msgId, conv_id: fromConvId },
    },
    { convId: toConvId },
  );
}

/** 举报。 */
export function sendReport(targetMsg: number, reason: number, detail = ''): void {
  sendFrame(Kind.REPORT, { target_msg: targetMsg, reason, detail });
}

/** 上报在线状态。 */
export function sendPresence(status: 'online' | 'away' | 'offline'): void {
  sendFrame(Kind.PRESENCE_UPDATE, { status, at: Date.now() });
}

/** 创建会话（kind 200 → 服务端翻译为 930 op1）。convType 用 ConvType 常量。 */
export function sendCreateConversation(input: {
  title?: string;
  convType?: number;
  memberIds?: number[];
}): boolean {
  return sendFrame(Kind.CONV_CREATE, {
    title: input.title ?? '',
    convType: input.convType ?? ConvType.DIRECT,
    memberIds: input.memberIds ?? [],
  });
}

/** 成员增删（kind 210 / 211）。 */
export function sendMemberOp(convId: number, userIds: number[], add: boolean): boolean {
  return sendFrame(add ? Kind.MEMBER_ADD : Kind.MEMBER_REMOVE, { convId, userIds }, { convId });
}

/** 更新自己的资料（kind 940 op1）。 */
export function requestUpdateProfile(displayName: string): Promise<{ ok: boolean; body?: unknown }> {
  return requestWithReply(Kind.IDENTITY_REQUEST, { op: 1, displayName });
}

/** 读取用户资料（kind 940 op2）。 */
export function requestGetUser(userId: number): Promise<{ ok: boolean; body?: unknown }> {
  return requestWithReply(Kind.IDENTITY_REQUEST, { op: 2, userId });
}

/** 列出我的会话（kind 930 op7）。 */
export function requestListMyConversations(): Promise<{ ok: boolean; body?: unknown }> {
  return requestWithReply(Kind.CONV_DATA_REQUEST, { op: 7, body: '' });
}

/** 搜索用户（kind 940 op5）。 */
export function requestSearchUsers(query: string): Promise<{ ok: boolean; body?: unknown }> {
  return requestWithReply(Kind.IDENTITY_REQUEST, { op: 5, query });
}

function handleFrame(frame: {
  kind: number;
  convId: number;
  userId: number;
  requestId: string;
  payload: Record<string, unknown> | null;
  rawPayload: string | null;
  ackId: number;
}): void {
  const msg = useMsgStore.getState();
  const conv = useConvStore.getState();
  const ui = useUiStore.getState();

  switch (frame.kind) {
    case Kind.MSG_NEW: {
      const payload = frame.payload ?? {};
      const convId = frame.convId || Number(payload.convId ?? payload.conv_id ?? 0);
      if (!convId) break;
      const lastSeq = msg.lastSeq[convId] ?? 0;
      const seq = Number(payload.seq ?? 0);
      // seq 出现缺口 → 立刻补拉，避免丢消息。
      if (lastSeq > 0 && seq > lastSeq + 1) {
        requestSync(convId, lastSeq);
      }
      msg.applyNew(convId, payload);
      const applied = useMsgStore.getState().listOf(convId).slice(-1)[0];
      if (applied) conv.touch(convId, applied as ChatMessage);
      break;
    }
    case Kind.MSG_ACK: {
      msg.applyAck(frame.payload ?? {});
      const clientMsgId = String(frame.payload?.clientMsgId ?? '');
      const convId = frame.convId;
      if (convId && clientMsgId) {
        const found = msg.listOf(convId).find((m) => m.clientMsgId === clientMsgId);
        if (found) conv.touch(convId, found);
      }
      break;
    }
    case Kind.MSG_READ:
    case Kind.MSG_DELIVERED: {
      const payload = frame.payload ?? {};
      const convId = frame.convId || Number(payload.conv_id ?? 0);
      if (convId) msg.applyReceipt(convId, payload);
      break;
    }
    case Kind.MSG_DELETE:
    case Kind.MSG_RECALL: {
      const payload = frame.payload ?? {};
      const convId = frame.convId || Number(payload.conv_id ?? 0);
      const msgId = Number(payload.msg_id ?? payload.msgId ?? 0);
      if (convId && msgId) msg.applyTombstone(convId, msgId, true);
      break;
    }
    case Kind.CONV_PAYLOAD: {
      conv.upsertFromPayload(frame.payload ?? {});
      break;
    }
    case Kind.CONV_DATA_RESULT:
    case Kind.IDENTITY_RESULT: {
      const payload = frame.payload ?? {};
      const ok = Boolean(payload.ok);
      const errorText = typeof payload.error === 'string' ? payload.error : '';
      const requestId = typeof payload.requestId === 'string' ? payload.requestId : frame.requestId;
      const bodyRaw = typeof payload.body === 'string' ? payload.body : '';
      let body: unknown;
      if (bodyRaw) {
        const decoded = decodePayload(bodyRaw);
        body = decoded.payload;
      }
      // 先结掉等待中的请求（带 request_id 的那一路）。
      if (requestId && settlePending(requestId, ok, errorText, body)) break;
      // 服务端主动下发（无 request_id）：931 的 body 是会话快照，直接补进列表。
      if (ok && body && typeof body === 'object') {
        const snapshot = body as { convId?: number; items?: unknown[] };
        if (Array.isArray(snapshot.items)) {
          for (const item of snapshot.items) {
            conv.upsertFromPayload(item as Record<string, unknown>);
          }
        } else if (snapshot.convId) {
          conv.upsertFromPayload(body as Record<string, unknown>);
        }
      } else if (!ok && errorText) {
        ui.pushToast(`操作失败：${errorText}`, 'error');
      }
      break;
    }
    case Kind.WATERMARK: {
      const payload = frame.payload ?? {};
      const convId = Number(payload.convId ?? payload.conv_id ?? frame.convId ?? 0);
      if (!convId) break;
      const latest = Number(payload.latestSeq ?? payload.latest_seq ?? 0);
      const lastSeq = msg.lastSeq[convId] ?? 0;
      if (latest > lastSeq) requestSync(convId, lastSeq);
      break;
    }
    case Kind.TYPING: {
      const payload = frame.payload ?? {};
      const convId = Number(payload.conv_id ?? frame.convId ?? 0);
      const userId = Number(payload.user_id ?? 0);
      if (convId && userId) {
        msg.setTyping(convId, userId, Boolean(payload.typing));
      }
      break;
    }
    case Kind.PONG: {
      const payload = decodePayload(frame.rawPayload ?? undefined);
      const clientMs = Number((payload.payload as { client_ms?: number } | null)?.client_ms ?? 0);
      if (clientMs > 0) {
        useConnStore.getState().setState('open');
        useConnStore.setState({ latencyMs: Date.now() - clientMs });
      }
      break;
    }
    case Kind.RATE_LIMITED: {
      const payload = frame.payload ?? {};
      const retryAfterMs = Number(payload.retry_after_ms ?? payload.retryAfterMs ?? 500);
      useConnStore.setState({ detail: `服务端限流，${retryAfterMs}ms 后重试` });
      ui.pushToast('发送过于频繁，请稍后再试', 'warn');
      break;
    }
    case Kind.KICKED: {
      const payload = frame.payload ?? {};
      const reason = String(payload.reason ?? '你已被管理员下线');
      useConnStore.setState({ kickedReason: reason });
      ui.pushToast(reason, 'error');
      void useAuthStore.getState().logout();
      break;
    }
    case Kind.ERROR: {
      const payload = frame.payload ?? {};
      const reason = String(payload.reason ?? '未知错误');
      useConnStore.setState({ detail: `服务端错误：${reason}` });
      ui.pushToast(`服务端错误：${reason}`, 'error');
      break;
    }
    default:
      // 未知 kind 静默忽略：服务端会继续扩展号段。
      break;
  }
}
