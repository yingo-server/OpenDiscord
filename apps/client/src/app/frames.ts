import type { Frame } from '../net/protocol/frame';
import { K, isRecord, isAckSuccess } from '../net';
import { getState, addMessage, patchMessage, upsertConversation, setTyping } from '../state/chat';
import { parseConversation, parseMessage } from '../domain/parse';

type SyncFn = (convId: number, sinceSeq: number) => void;
type PendingMap = Map<string, { msg: { convId: number; attempts: number }; timer: number }>;

export function handleFrame(frame: Frame, ownId: number, pending: PendingMap, syncConversation: SyncFn): void {
  const payload = isRecord(frame.payload) ? frame.payload : {};
  const convId = frame.conv_id || 0;
  if (frame.kind === K.MSG_ACK) {
    const clientId = String(payload.clientMsgId ?? payload.client_msg_id ?? '');
    const entry = pending.get(clientId);
    if (!entry) return;
    clearTimeout(entry.timer);
    if (!isAckSuccess(frame)) {
      patchMessage(entry.msg.convId, clientId, { status: 'failed', pending: false, error: 'send' });
    } else {
      patchMessage(entry.msg.convId, clientId, {
        id: String(payload.msgId ?? payload.msg_id ?? clientId),
        seq: typeof payload.seq === 'number' ? payload.seq : 0,
        status: 'sent', pending: false,
      });
    }
    pending.delete(clientId);
    return;
  }
  if (frame.kind === K.MSG_NEW) {
    const message = parseMessage(frame.payload, convId, ownId);
    if (!message.id) return;
    const existing = (getState().messages[convId] || []).find((m) => m.id === message.id || (message.seq > 0 && m.seq === message.seq));
    if (!existing) addMessage(convId, message);
    const conv = getState().conversations.find((c) => c.id === convId);
    if (!conv && convId) {
      const inferred = parseConversation({ id: convId, type: 'direct', last_text: message.text, last_at: message.createdAt, last_seq: message.seq });
      if (inferred) upsertConversation(inferred);
    }
    return;
  }
  if (frame.kind === K.CONV_PAYLOAD || frame.kind === 931) {
    const conv = parseConversation(frame.payload);
    if (conv) upsertConversation(conv);
    return;
  }
  if (frame.kind === K.TYPING) {
    setTyping(convId, Number(payload.userId ?? payload.user_id ?? 0), payload.typing === true);
    return;
  }
  if (frame.kind === K.WATERMARK) {
    const latest = Number(payload.latestSeq ?? payload.latest_seq ?? 0);
    if (convId && latest) syncConversation(convId, latest);
    return;
  }
  if (frame.kind === K.KICKED) {
    window.dispatchEvent(new CustomEvent('ufot:connection', { detail: { state: 'banned' } }));
  }
  // ERROR / RATE_LIMITED and unknown kinds stay silent to users.
}
