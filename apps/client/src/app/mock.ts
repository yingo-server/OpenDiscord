/**
 * 开发期 mock（VITE_MOCK=1）：不连后端即可预览主界面。
 * 只在生产构建之外生效判断由 env.mock 控制（默认 false，生产不受影响）。
 */
import type { Session, Conversation } from '../domain/types';
import { parseConversation, parseMessage } from '../domain/parse';
import { addMessage, upsertConversation } from '../state/chat';
import type { AppServices } from './services';

export const MOCK_SESSION: Session = {
  user_id: 101,
  device_id: 'mock-device',
  access_token: 'mock-access-token',
  token: 'mock-access-token',
  refresh_token: 'mock-refresh-token',
  expires_at: Math.floor(Date.now() / 1000) + 3600,
  expires_at_unix: Math.floor(Date.now() / 1000) + 3600,
  session_id: 'mock-session',
  guest: false,
  is_new_user: false,
  profile: { display_name: '预览用户', status: 'mock' },
};

function mockConversations(): void {
  const now = Date.now();
  upsertConversation(parseConversation({ id: 1, type: 'direct', title: '产品讨论组', last_seq: 3, unread: 1,
    last_at: now - 1000 * 60 * 3, member_ids: [101, 102],
    last_text: '明早十点对齐一下排期',
  }) as Conversation);
  upsertConversation(parseConversation({
    id: 2, type: 'group', title: 'UFOT 内测群', last_seq: 2, pinned: true,
    last_at: now - 1000 * 60 * 60, member_ids: [101, 102, 103],
    last_text: '新版本看起来不错',
  }) as Conversation);
  addMessage(1, parseMessage({
    id: 'm1', seq: 1, sender_id: 102, created_at: now - 1000 * 60 * 5,
    payload: { text: '今天的设计稿我发到群里了，帮忙看下' },
  }, 1, 102));
  addMessage(1, parseMessage({
    id: 'm2', seq: 2, sender_id: 101, created_at: now - 1000 * 60 * 4,
    payload: { text: '收到，整体方向没问题，细节我标了几条批注' },
  }, 1, 101));
  addMessage(1, parseMessage({
    id: 'm3', seq: 3, sender_id: 102, created_at: now - 1000 * 60 * 3,
    payload: { text: '明早十点对齐一下排期' },
  }, 1, 102));
}

/** 模拟对端回声：本地发送的消息稍后由"对方"回一条，模拟 MSG_NEW。 */
function installEcho(services: AppServices): void {
  const original = services.sendMessage.bind(services);
  services.sendMessage = (input) => {
    const mine = original(input);
    window.setTimeout(() => {
      addMessage(input.convId, parseMessage({
        id: `echo-${mine.id}`, seq: 1000 + Math.floor(Math.random() * 9000),
        sender_id: 102, created_at: Date.now(),
        payload: { text: `收到：${mine.text}` },
      }, input.convId, 102));
    }, 1200);
    return mine;
  };
}

export function applyMock(services: AppServices): void {
  mockConversations();
  installEcho(services);
  // 模拟登录（auth 类方法直接返回假会话）
  services.auth.guest = () => Promise.resolve({ ...MOCK_SESSION });
  services.auth.password = () => Promise.resolve({ ...MOCK_SESSION });
  services.auth.phoneVerify = () => Promise.resolve({ ...MOCK_SESSION });
  services.auth.phoneCode = () => Promise.resolve({ sent: true, expires_in: 300, resend_after: 60 });
  services.auth.emailLink = () => Promise.resolve({ sent: true, expires_in: 900, poll_after: 5 });
  services.auth.emailPoll = () => Promise.resolve({ status: 'pending' });
  services.auth.refresh = () => Promise.resolve({ ...MOCK_SESSION });
  services.auth.logout = () => Promise.resolve({ ok: true });
  services.auth.setPassword = () => Promise.resolve({ ok: true });
  services.connect = () => undefined;
  services.disconnect = () => undefined;
  services.updateProfile = () => Promise.resolve();
  window.dispatchEvent(new CustomEvent('ufot:connection', { detail: { state: 'online' } }));
}

/** mock 模式下模拟收到新消息，供测试与预览触发。 */
export function mockIncoming(convId: number, text: string): void {
  addMessage(convId, parseMessage({
    id: `mock-${Date.now()}`, seq: Date.now(), sender_id: 102, created_at: Date.now(),
    payload: { text },
  }, convId, 102));
}
