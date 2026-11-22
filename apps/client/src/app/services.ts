import type { ChatMessage, Conversation, Session } from '../domain/types';
import type { PendingMessage } from '../net';
import type { HttpClient } from '../net/http/client';
import type { AuthApi, MediaApi } from '../net/http/auth';
import type { WsClient } from '../net/ws/client';
import type { WsRequests } from '../net/ws/requests';
import type { Frame } from '../net/protocol/frame';
import { getState } from '../state/chat';

export interface AppServices {
  http: HttpClient;
  auth: AuthApi;
  media: MediaApi;
  ws: WsClient;
  session: () => Session | null;
  setSession: (session: Session | null) => void;
  authenticate: (session: Session) => void;
  logout: () => Promise<void>;
  connect: () => void;
  disconnect: () => void;
  sendMessage: (input: Omit<PendingMessage, 'attempts' | 'sentAt'>) => ChatMessage;
  retryMessage: (convId: number, id: string) => void;
  editMessage: (convId: number, id: string, text: string) => void;
  deleteMessage: (convId: number, id: string) => void;
  recallMessage: (convId: number, id: string) => void;
  forwardMessage: (fromConv: number, id: string, toConv: number) => void;
  sendRead: (convId: number, seq: number) => void;
  sendTyping: (convId: number, typing: boolean) => void;
  createConversation: (type: 'direct' | 'group', memberIds: number[], title: string) => void;
  selectConversation: (id: number | null) => void;
  syncConversation: (convId: number, sinceSeq: number) => void;
  upload: (file: File) => Promise<string>;
  updateProfile: (input: { display_name: string; avatar_url?: string }) => Promise<void>;
  requests: WsRequests;
  handleFrame: (frame: Frame) => void;
}

let services: AppServices | null = null;

export function getAppServices(): AppServices {
  if (!services) throw new Error('Services not initialized');
  return services;
}

export function setAppServices(value: AppServices): void {
  services = value;
}

export function conversations(): Conversation[] {
  return getState().conversations;
}

export function messagesFor(convId: number): ChatMessage[] {
  return getState().messages[convId] || [];
}
