import type { Conversation, ChatMessage } from '../domain/types';

export interface ChatState {
  conversations: Conversation[];
  messages: Record<number, ChatMessage[]>;
  activeConvId: number | null;
  unreadTotal: number;
  connected: boolean;
  typing: Record<number, number[]>;
  draft: Record<number, string>;
}

type Listener = (state: ChatState) => void;

const initial: ChatState = {
  conversations: [],
  messages: {},
  activeConvId: null,
  unreadTotal: 0,
  connected: false,
  typing: {},
  draft: {},
};

let state: ChatState = initial;
const listeners = new Set<Listener>();

function emit(): void {
  for (const listener of listeners) listener(state);
}

export function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  listener(state);
  return () => listeners.delete(listener);
}

export function getState(): ChatState {
  return state;
}

export function setState(patch: Partial<ChatState>): void {
  state = { ...state, ...patch };
  emit();
}

export function resetState(): void {
  state = { ...initial, conversations: [], messages: {}, typing: {}, draft: {} };
  emit();
}

export function upsertConversation(conv: Conversation): void {
  const index = state.conversations.findIndex((c) => c.id === conv.id);
  const conversations = index === -1
    ? [...state.conversations, conv]
    : state.conversations.map((c) => (c.id === conv.id ? { ...c, ...conv } : c));
  conversations.sort((a, b) => Number(b.pinned) - Number(a.pinned) || (b.lastAt || 0) - (a.lastAt || 0));
  const unreadTotal = conversations.reduce((sum, c) => sum + c.unread, 0);
  setState({ conversations, unreadTotal });
}

export function removeConversation(id: number): void {
  const conversations = state.conversations.filter((c) => c.id !== id);
  const messages = { ...state.messages };
  delete messages[id];
  setState({ conversations, messages, unreadTotal: conversations.reduce((sum, c) => sum + c.unread, 0) });
}

export function setActiveConv(id: number | null): void {
  if (id !== null) {
    const conversations = state.conversations.map((c) => (c.id === id ? { ...c, unread: 0 } : c));
    setState({ activeConvId: id, conversations, unreadTotal: conversations.reduce((sum, c) => sum + c.unread, 0) });
  } else {
    setState({ activeConvId: null });
  }
}

function sortMessages(messages: ChatMessage[]): ChatMessage[] {
  return [...messages].sort((a, b) => (a.createdAt || a.seq) - (b.createdAt || b.seq));
}

export function addMessage(convId: number, message: ChatMessage): void {
  const list = state.messages[convId] || [];
  const index = list.findIndex((m) => m.id === message.id || (message.seq > 0 && m.seq === message.seq));
  const next = index === -1 ? [...list, message] : list.map((m, i) => (i === index ? { ...m, ...message } : m));
  const conversations = state.conversations.map((c) => {
    if (c.id !== convId) return c;
    const isActive = state.activeConvId === convId;
    const unread = isActive || message.senderId === 0 ? c.unread : c.unread + 1;
    return {
      ...c,
      unread,
      lastSeq: Math.max(c.lastSeq, message.seq),
      lastText: message.text || (message.media.length ? '[附件]' : ''),
      lastAt: message.createdAt || Date.now(),
    };
  });
  setState({ messages: { ...state.messages, [convId]: sortMessages(next) }, conversations });
}

export function patchMessage(convId: number, id: string, patch: Partial<ChatMessage>): void {
  const list = state.messages[convId] || [];
  const next = list.map((m) => (m.id === id ? { ...m, ...patch } : m));
  setState({ messages: { ...state.messages, [convId]: next } });
}

export function removeMessage(convId: number, id: string): void {
  const list = (state.messages[convId] || []).filter((m) => m.id !== id);
  setState({ messages: { ...state.messages, [convId]: list } });
}

export function setTyping(convId: number, userId: number, typing: boolean): void {
  const set = new Set<number>(state.typing[convId] || []);
  if (typing) set.add(userId);
  else set.delete(userId);
  setState({ typing: { ...state.typing, [convId]: [...set] } });
}

export function setDraft(convId: number, text: string): void {
  setState({ draft: { ...state.draft, [convId]: text } });
}
