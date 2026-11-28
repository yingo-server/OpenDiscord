/**
 * 消息状态：乐观发送、ACK 配对、seq 缺口检测与补拉、墓碑渲染。
 *
 * 后端事实：
 * - MSG_SEND(101) → MSG_ACK(103){msgId, seq, clientMsgId, status}；
 * - MSG_NEW(102){msgId, seq, senderId, content(base64 JSON), sentAt}；
 * - SYNC_REQUEST(8){convId, sinceSeq, limit} 应答是**逐条 102**（不是 9）；
 * - 水位线 WATERMARK(14) 表示服务端 seq 已领先，客户端需补拉缺口。
 */

import { create } from 'zustand';
import type { ChatMessage, MediaItem, MessageContent, MessageStatus } from '@/core/domain/types';
import { decodePayload } from '@/core/net/ws/codec';

interface MsgState {
  messages: Record<number, ChatMessage[]>;
  /** 每个会话已确认的最大 seq（缺口检测基准） */
  lastSeq: Record<number, number>;
  /** 已读水位（本地记录，服务端目前不回读状态） */
  readSeq: Record<number, number>;
  /** 正在输入的用户 */
  typing: Record<number, number[]>;

  appendLocal: (message: ChatMessage) => void;
  applyAck: (payload: Record<string, unknown>) => void;
  applyNew: (convId: number, payload: Record<string, unknown>) => void;
  applyTombstone: (convId: number, msgId: number, tombstone: boolean) => void;
  applyReceipt: (convId: number, payload: Record<string, unknown>) => void;
  setStatus: (convId: number, msgId: number, status: MessageStatus) => void;
  updateText: (convId: number, msgId: number, text: string) => void;
  setTyping: (convId: number, userId: number, typing: boolean) => void;
  listOf: (convId: number) => ChatMessage[];
  markRead: (convId: number, msgId: number) => void;
}

let localIdSeed = -1;
export function nextLocalId(): number {
  localIdSeed -= 1;
  return localIdSeed;
}

function readContent(content: unknown): MessageContent {
  if (!content) return {};
  if (typeof content === 'string') {
    const decoded = decodePayload(content);
    return (decoded.payload ?? {}) as MessageContent;
  }
  if (typeof content === 'object') return content as MessageContent;
  return {};
}

function toMedia(raw: unknown): MediaItem[] | undefined {
  if (!Array.isArray(raw)) return undefined;
  const items = raw
    .map((item): MediaItem | null => {
      const rec = (item ?? {}) as Record<string, unknown>;
      const mediaId = String(rec.mediaId ?? rec.media_id ?? '');
      if (!mediaId) return null;
      return {
        mediaId,
        mtype: Number(rec.mtype ?? 4),
        name: typeof rec.name === 'string' ? rec.name : undefined,
        size: typeof rec.size === 'number' ? rec.size : undefined,
        mime: typeof rec.mime === 'string' ? rec.mime : undefined,
        width: typeof rec.width === 'number' ? rec.width : undefined,
        height: typeof rec.height === 'number' ? rec.height : undefined,
        durationMs: typeof rec.durationMs === 'number' ? rec.durationMs : undefined,
      };
    })
    .filter((item): item is MediaItem => item !== null);
  return items.length > 0 ? items : undefined;
}

export const useMsgStore = create<MsgState>((set, get) => ({
  messages: {},
  lastSeq: {},
  readSeq: {},
  typing: {},

  appendLocal(message) {
    const list = get().messages[message.convId] ?? [];
    set({
      messages: {
        ...get().messages,
        [message.convId]: [...list, message].sort((a, b) => a.sentAt - b.sentAt),
      },
    });
  },

  applyAck(payload) {
    const clientMsgId = String(payload.clientMsgId ?? payload.client_msg_id ?? '');
    const msgId = Number(payload.msgId ?? payload.msg_id ?? 0);
    const seq = Number(payload.seq ?? 0);
    const status = Number(payload.status ?? 0);
    if (!clientMsgId) return;

    for (const [convKey, list] of Object.entries(get().messages)) {
      const convId = Number(convKey);
      const index = list.findIndex((m) => m.clientMsgId === clientMsgId);
      if (index < 0) continue;
      const target = list[index];
      if (!target) continue;
      const next = [...list];
      next[index] = {
        ...target,
        msgId: msgId || target.msgId,
        seq: seq || target.seq,
        status: (status === 0 ? 'sent' : 'failed') as MessageStatus,
      };
      set({
        messages: { ...get().messages, [convId]: next },
        lastSeq: { ...get().lastSeq, [convId]: Math.max(get().lastSeq[convId] ?? 0, seq) },
      });
      return;
    }
  },

  applyNew(convId, payload) {
    const content = readContent(payload.content);
    const msgId = Number(payload.msgId ?? payload.msg_id ?? 0);
    const seq = Number(payload.seq ?? 0);
    const senderId = Number(payload.senderId ?? payload.sender_id ?? 0);
    const sentAt = Number(payload.sentAt ?? payload.sent_at ?? Date.now());
    const list = get().messages[convId] ?? [];
    if (list.some((m) => m.msgId === msgId)) return;

    const forwardFrom = content.forward_from;

    const message: ChatMessage = {
      msgId,
      convId,
      seq,
      senderId,
      text: content.text ?? '',
      media: toMedia(content.media),
      replyTo: Number(content.replyTo ?? content.reply_to ?? 0) || undefined,
      mentions: Array.isArray(content.mentions) ? content.mentions.map(Number) : undefined,
      sentAt,
      status: 'sent',
      clientMsgId: content.clientMsgId,
      forwardFrom: forwardFrom?.msgId || forwardFrom?.msg_id
        ? {
            msgId: Number(forwardFrom.msgId ?? forwardFrom.msg_id),
            convId: Number(forwardFrom.convId ?? forwardFrom.conv_id ?? convId),
          }
        : undefined,
    };
    set({
      messages: { ...get().messages, [convId]: [...list, message].sort((a, b) => a.sentAt - b.sentAt) },
      lastSeq: { ...get().lastSeq, [convId]: Math.max(get().lastSeq[convId] ?? 0, seq) },
    });
  },

  applyTombstone(convId, msgId, tombstone) {
    const list = get().messages[convId];
    if (!list) return;
    const next = list.map((m) => (m.msgId === msgId ? { ...m, tombstone } : m));
    set({ messages: { ...get().messages, [convId]: next } });
  },

  applyReceipt(convId, payload) {
    const msgId = Number(payload.msg_id ?? payload.msgId ?? 0);
    const rtype = Number(payload.rtype ?? 0);
    if (!msgId) return;
    const list = get().messages[convId];
    if (!list) return;
    const next = list.map((m) => {
      if (m.msgId !== msgId) return m;
      if (rtype === 0) return m.status === 'read' ? m : { ...m, status: 'read' as MessageStatus };
      if (m.status === 'sent') return { ...m, status: 'delivered' as MessageStatus };
      return m;
    });
    set({ messages: { ...get().messages, [convId]: next } });
  },

  setStatus(convId, msgId, status) {
    const list = get().messages[convId];
    if (!list) return;
    set({
      messages: {
        ...get().messages,
        [convId]: list.map((m) => (m.msgId === msgId ? { ...m, status } : m)),
      },
    });
  },

  updateText(convId, msgId, text) {
    const list = get().messages[convId];
    if (!list) return;
    set({
      messages: {
        ...get().messages,
        [convId]: list.map((m) => (m.msgId === msgId ? { ...m, text } : m)),
      },
    });
  },

  setTyping(convId, userId, typing) {
    const current = new Set(get().typing[convId] ?? []);
    if (typing) current.add(userId);
    else current.delete(userId);
    set({ typing: { ...get().typing, [convId]: [...current] } });
  },

  listOf(convId) {
    return get().messages[convId] ?? [];
  },

  markRead(convId, msgId) {
    if (!msgId) return;
    set({ readSeq: { ...get().readSeq, [convId]: Math.max(get().readSeq[convId] ?? 0, msgId) } });
  },
}));
