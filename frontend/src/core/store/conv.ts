/**
 * 会话状态。
 *
 * 数据来源：
 *  1) 930 op7「列出我的会话」——进应用/刷新时主动拉一次（权威来源）；
 *  2) WS kind 202 CONV_PAYLOAD —— 成员变更时的增量推送；
 *  3) 收到 102 消息时补一个会话条目（此前会话没被列举到的情况）。
 * 置顶/静音是客户端本地偏好（服务端没有这个概念），持久化在 localStorage。
 */

import { create } from 'zustand';
import type { ChatMessage, Conversation } from '@/core/domain/types';

const PREFS_KEY = 'ufot.conv.prefs';

interface ConvPrefs {
  pinned: number[];
  muted: number[];
}

function loadPrefs(): ConvPrefs {
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return { pinned: [], muted: [] };
    const parsed = JSON.parse(raw) as ConvPrefs;
    return { pinned: parsed.pinned ?? [], muted: parsed.muted ?? [] };
  } catch {
    return { pinned: [], muted: [] };
  }
}

function savePrefs(prefs: ConvPrefs): void {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
  } catch {
    /* 忽略 */
  }
}

/** 服务端 ConvPayload → 前端 Conversation。 */
export function conversationFromPayload(payload: Record<string, unknown>): Conversation | null {
  const convId = Number(payload.convId ?? payload.conv_id ?? 0);
  if (!convId) return null;
  const members = Array.isArray(payload.members) ? payload.members : [];
  return {
    convId,
    title: typeof payload.title === 'string' ? payload.title : '',
    convType: Number(payload.convType ?? payload.conv_type ?? 0),
    createdAt: Number(payload.createdAt ?? payload.created_at ?? 0),
    members: members.map((m) => {
      const rec = (m ?? {}) as Record<string, unknown>;
      return {
        userId: Number(rec.userId ?? rec.user_id ?? 0),
        role: Number(rec.role ?? 0),
        joinedAt: Number(rec.joinedAt ?? 0),
        mutedUntil: Number(rec.mutedUntil ?? 0),
      };
    }),
    memberCount: Number(payload.memberCount ?? 0) || undefined,
    membersOmitted: Boolean(payload.membersOmitted),
    version: Number(payload.version ?? 0) || undefined,
    lastSeq: Number(payload.lastSeq ?? 0) || undefined,
    unread: Number(payload.unread ?? 0) || undefined,
  };
}

interface ConvState {
  conversations: Record<number, Conversation>;
  order: number[];
  loading: boolean;
  error: string | null;
  /** 是否已成功拉取过完整列表（决定空态文案） */
  loaded: boolean;
  query: string;

  replaceAll: (conversations: Conversation[]) => void;
  upsertFromPayload: (payload: Record<string, unknown>) => void;
  touch: (convId: number, message: ChatMessage) => void;
  setQuery: (query: string) => void;
  togglePinned: (convId: number) => void;
  toggleMuted: (convId: number) => void;
  visible: () => Conversation[];
}

export const useConvStore = create<ConvState>((set, get) => {
  const prefs = loadPrefs();

  const applyPrefs = (conv: Conversation): Conversation => ({
    ...conv,
    pinned: prefs.pinned.includes(conv.convId),
    muted: prefs.muted.includes(conv.convId),
  });

  return {
    conversations: {},
    order: [],
    loading: false,
    error: null,
    loaded: false,
    query: '',

    replaceAll(conversations) {
      const next: Record<number, Conversation> = {};
      for (const conv of conversations) next[conv.convId] = applyPrefs(conv);
      set({
        conversations: next,
        order: conversations.map((c) => c.convId),
        loaded: true,
      });
    },

    upsertFromPayload(payload) {
      const parsed = conversationFromPayload(payload);
      if (!parsed) return;
      const current = get().conversations[parsed.convId];
      const merged = applyPrefs({
        ...current,
        ...parsed,
        members: parsed.members.length > 0 ? parsed.members : (current?.members ?? []),
        // 标题不覆盖已有的非空标题（增量事件可能不带标题）
        title: parsed.title || current?.title || '',
      });
      const order = get().order.includes(merged.convId)
        ? get().order
        : [merged.convId, ...get().order];
      set({
        conversations: { ...get().conversations, [merged.convId]: merged },
        order,
        loaded: true,
      });
    },

    touch(convId, message) {
      const current = get().conversations[convId];
      if (!current) {
        get().upsertFromPayload({ convId, title: '', convType: 0 });
        const created = get().conversations[convId];
        if (!created) return;
        set({
          conversations: {
            ...get().conversations,
            [convId]: { ...created, lastMessage: message, lastActivity: message.sentAt },
          },
        });
        return;
      }
      set({
        conversations: {
          ...get().conversations,
          [convId]: { ...current, lastMessage: message, lastActivity: message.sentAt },
        },
      });
    },

    setQuery(query) {
      set({ query });
    },

    togglePinned(convId) {
      const prefsNow = loadPrefs();
      const pinned = prefsNow.pinned.includes(convId)
        ? prefsNow.pinned.filter((id) => id !== convId)
        : [...prefsNow.pinned, convId];
      savePrefs({ ...prefsNow, pinned });
      const conv = get().conversations[convId];
      if (conv) {
        set({
          conversations: {
            ...get().conversations,
            [convId]: { ...conv, pinned: pinned.includes(convId) },
          },
        });
      }
    },

    toggleMuted(convId) {
      const prefsNow = loadPrefs();
      const muted = prefsNow.muted.includes(convId)
        ? prefsNow.muted.filter((id) => id !== convId)
        : [...prefsNow.muted, convId];
      savePrefs({ ...prefsNow, muted });
      const conv = get().conversations[convId];
      if (conv) {
        set({
          conversations: {
            ...get().conversations,
            [convId]: { ...conv, muted: muted.includes(convId) },
          },
        });
      }
    },

    visible() {
      const { conversations, order, query } = get();
      const keyword = query.trim().toLowerCase();
      const list = order
        .map((id) => conversations[id])
        .filter((conv): conv is Conversation => Boolean(conv));
      const filtered = keyword
        ? list.filter(
            (conv) =>
              conv.title.toLowerCase().includes(keyword) ||
              conv.lastMessage?.text?.toLowerCase().includes(keyword),
          )
        : list;
      return [...filtered].sort((a, b) => {
        if (Boolean(a.pinned) !== Boolean(b.pinned)) return a.pinned ? -1 : 1;
        const aTime = a.lastActivity ?? a.lastSeq ?? 0;
        const bTime = b.lastActivity ?? b.lastSeq ?? 0;
        return bTime - aTime;
      });
    },
  };
});
