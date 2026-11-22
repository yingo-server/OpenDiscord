import { env } from '../platform/env';
import { HttpClient, ApiError, AuthApi, MediaApi, WsClient, K, pendingSendFrame, retryTimeout } from '../net';
import type { PendingMessage } from '../net';
import type { ChatMessage, Session } from '../domain/types';
import { setSession, getSession, storedRefreshToken } from '../state/session';
import { parseSession } from '../domain/parse';
import { setAppServices, type AppServices } from './services';
import { handleFrame } from './frames';
import { applyMock, MOCK_SESSION } from './mock';
import { WsRequests } from '../net/ws/requests';
import { IdentityApi } from '../net/ws/identity';
import { callSession } from '../net/ws/call';
import { getState, setState, addMessage, patchMessage, removeMessage, setActiveConv } from '../state/chat';

export function bootstrap(): AppServices {
  const http = new HttpClient(env.apiBase);
  const auth = new AuthApi(http);
  const media = new MediaApi(http);
  const ws = new WsClient(env.wsUrl);
  const requests = new WsRequests((frame) => ws.send(frame as never));
  const identity = new IdentityApi(requests);
  ws.onFrame((frame) => { requests.handle(frame); });
  const pending = new Map<string, { msg: PendingMessage; timer: number }>();
  const ownId = () => getSession()?.user_id || 0;

  function dispatchSend(msg: PendingMessage): void {
    ws.send(pendingSendFrame(msg));
    const timer = window.setTimeout(() => {
      const entry = pending.get(msg.clientMsgId);
      if (!entry) return;
      if (entry.msg.attempts >= 2) {
        patchMessage(msg.convId, msg.clientMsgId, { status: 'failed', pending: false, error: 'network' });
        pending.delete(msg.clientMsgId);
        return;
      }
      entry.msg.attempts += 1;
      dispatchSend(entry.msg);
    }, retryTimeout(msg.attempts));
    pending.set(msg.clientMsgId, { msg, timer });
  }

  function sendMessage(input: Omit<PendingMessage, 'attempts' | 'sentAt'>): ChatMessage {
    const optimistic: ChatMessage = {
      id: input.clientMsgId, seq: 0, convId: input.convId, senderId: ownId(),
      text: input.text, createdAt: Date.now(), status: 'sending',
      replyTo: input.replyTo, mentions: input.mentions,
      media: input.media.map((id) => ({ id, kind: 'file' })), pending: true,
    };
    addMessage(input.convId, optimistic);
    dispatchSend({ ...input, attempts: 0, sentAt: Date.now() });
    return optimistic;
  }

  function retryMessage(convId: number, id: string): void {
    const message = getState().messages[convId]?.find((m) => m.id === id);
    if (!message) return;
    patchMessage(convId, id, { status: 'sending', pending: true });
    const entry = pending.get(id);
    if (entry) {
      if (entry.timer) clearTimeout(entry.timer);
      entry.msg.attempts = 0;
      dispatchSend(entry.msg);
    } else {
      sendMessage({
        convId, clientMsgId: `retry-${Date.now()}`, text: message.text,
        mentions: message.mentions || [], media: message.media.map((m) => m.id || '').filter(Boolean),
      });
    }
  }

  function editMessage(convId: number, id: string, text: string): void {
    patchMessage(convId, id, { text, edited: true });
    ws.send({ kind: K.MSG_EDIT, conv_id: convId, request_id: `edit-${id}`, payload: { msgId: Number(id) || id, convId, text } });
  }

  function deleteMessage(convId: number, id: string): void {
    removeMessage(convId, id);
    ws.send({ kind: K.MSG_DELETE, conv_id: convId, request_id: `delete-${id}`, payload: { msgId: Number(id) || id, convId, forEveryone: false } });
  }

  function recallMessage(convId: number, id: string): void {
    patchMessage(convId, id, { recalled: true, text: '' });
    ws.send({ kind: K.MSG_RECALL, conv_id: convId, request_id: `recall-${id}`, payload: { msgId: Number(id) || id, convId } });
  }

  function forwardMessage(fromConv: number, id: string, toConv: number): void {
    const msg = getState().messages[fromConv]?.find((m) => m.id === id);
    if (!msg) return;
    sendMessage({
      convId: toConv, clientMsgId: `fwd-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      text: msg.text, mentions: [], media: msg.media.map((m) => m.id || '').filter(Boolean),
    });
  }

  function sendRead(convId: number, seq: number): void {
    ws.send({ kind: K.MSG_READ, conv_id: convId, payload: { convId, seq } });
  }

  function sendTyping(convId: number, typing: boolean): void {
    ws.send({ kind: K.TYPING, conv_id: convId, payload: { convId, typing } });
  }

  function createConversation(type: 'direct' | 'group', memberIds: number[], title: string): void {
    ws.send({
      kind: K.CONV_CREATE, request_id: `conv-${Date.now()}`,
      payload: { title, type, convType: type === 'group' ? 2 : 1, member_ids: memberIds, memberIds },
    });
  }

  function selectConversation(id: number | null): void {
    setActiveConv(id);
    if (id !== null) {
      const conv = getState().conversations.find((c) => c.id === id);
      if (conv?.lastSeq) sendRead(id, conv.lastSeq);
    }
  }

  function syncConversation(convId: number, sinceSeq: number): void {
    ws.send({ kind: K.SYNC_REQUEST, conv_id: convId, payload: { convId, sinceSeq, limit: 200, coldMerge: true } });
  }

  async function upload(file: File): Promise<string> {
    return media.upload(file);
  }

  async function updateProfile(input: { display_name: string; avatar_url?: string }): Promise<void> {
    await identity.updateProfile(input);
  }

  const handleFrameBound = (frame: import('../net').Frame) => {
    void callSession.handleFrame(frame).then((consumed) => {
      if (consumed) return;
      handleFrame(frame, ownId(), pending, syncConversation);
    });
  };
  ws.onFrame(handleFrameBound);
  callSession.configure(requests, env.callEnabled);

  // 刷新页面恢复会话：Cookie 里只有 access_token，refresh_token 存 sessionStorage（不进 localStorage）。
  const storedRefresh = storedRefreshToken();
  if (storedRefresh) {
    void auth.refresh(storedRefresh).then((value) => {
      const restored = parseSession(value);
      if (restored) {
        setSession(restored);
        ws.resume();
      }
    }).catch(() => undefined);
  }
  ws.onState((state) => {
    setState({ connected: state === 'online' });
    window.dispatchEvent(new CustomEvent('ufot:connection', { detail: { state } }));
    if (state === 'online') {
      for (const conv of getState().conversations) {
        if (conv.lastSeq) syncConversation(conv.id, conv.lastSeq);
      }
    }
  });
  window.addEventListener('ufot:unauthorized', () => {
    setSession(null);
    ws.close();
  });

  function authenticate(session: Session): void {
    setSession(session);
    ws.resume();
  }

  async function logout(): Promise<void> {
    const session = getSession();
    if (session?.refresh_token) await auth.logout(session.refresh_token).catch(() => undefined);
    ws.close();
    setSession(null);
  }

  const services: AppServices = {
    http, auth, media, ws,
    session: getSession, setSession, authenticate, logout,
    connect: () => ws.connect(), disconnect: () => ws.close(),
    sendMessage, retryMessage, editMessage, deleteMessage, recallMessage, forwardMessage,
    sendRead, sendTyping, createConversation, selectConversation, syncConversation, upload,
    updateProfile, requests,
    handleFrame: handleFrameBound,
  };
  setAppServices(services);
  if (env.mock) {
    applyMock(services);
    // mock 下自动进入主界面（并支持刷新恢复）
    if (storedRefreshToken() || sessionStorage.getItem('ufot.mock.entered')) {
      sessionStorage.setItem('ufot.mock.entered', '1');
      setSession({ ...MOCK_SESSION });
    }
  }
  return services;
}
