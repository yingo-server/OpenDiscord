import type { Frame } from './protocol/frame';
import { K } from './protocol/kinds';
import { isRecord } from './protocol/frame';
import { retryDelay } from '../domain/backoff';

export { K } from './protocol/kinds';
export { decodeFrame, encodeFrame, isRecord } from './protocol/frame';
export type { Frame } from './protocol/frame';
export { HttpClient, ApiError } from './http/client';
export { subscribePush, pushAvailable } from './http/push';
export { AuthApi, MediaApi } from './http/auth';
export { WsClient } from './ws/client';
export { WsRequests, parseIdentityResult, type WsPendingResult } from './ws/requests';
export { IdentityApi, type StoreUser } from './ws/identity';

export interface PendingMessage {
  convId: number;
  clientMsgId: string;
  text: string;
  replyTo?: string;
  mentions: number[];
  media: string[];
  attempts: number;
  sentAt: number;
}

export function parseAttachments(values: unknown[]): Frame[] {
  return values.filter(isRecord).map((item) => ({ id: 0, kind: 0, payload: item }));
}

export function isAckSuccess(frame: Frame): boolean {
  if (!isRecord(frame.payload)) return true;
  return typeof frame.payload.status === 'number' ? frame.payload.status === 0 : true;
}

export function pendingSendFrame(msg: PendingMessage): Omit<Frame, 'id' | 'sent_at'> {
  return {
    kind: K.MSG_SEND,
    conv_id: msg.convId,
    request_id: msg.clientMsgId,
    payload: {
      clientMsgId: msg.clientMsgId,
      client_msg_id: msg.clientMsgId,
      text: msg.text,
      replyTo: msg.replyTo ? Number(msg.replyTo) : undefined,
      mentions: msg.mentions,
      media: msg.media.map((id) => ({ mediaId: id, media_id: id })),
    },
  };
}

export function retryTimeout(attempts: number, retryAfterMs?: number): number {
  return retryDelay(attempts, retryAfterMs);
}
