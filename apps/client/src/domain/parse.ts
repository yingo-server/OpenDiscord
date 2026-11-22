import type { Attachment, ChatMessage, Conversation, Session } from './types';
import { isRecord } from '../net/protocol/frame';

function asString(value: unknown, fallback = ''): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number') return String(value);
  return fallback;
}

function asInt(value: unknown, fallback = 0): number {
  if (typeof value === 'number' && Number.isFinite(value)) return Math.floor(value);
  if (typeof value === 'string' && /^\d+$/.test(value)) return Number(value);
  return fallback;
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

export function parseSession(value: unknown): Session | null {
  if (!isRecord(value)) return null;
  const userId = asInt(value.user_id ?? value.userId);
  const access = asString(value.access_token ?? value.token);
  if (!userId || !access) return null;
  const profile = isRecord(value.profile) ? value.profile : {};
  return {
    user_id: userId,
    device_id: asString(value.device_id ?? value.deviceId),
    access_token: access,
    token: asString(value.token) || access,
    refresh_token: asString(value.refresh_token ?? value.refreshToken),
    expires_at: asInt(value.expires_at ?? value.expires_at_unix ?? value.expiresAt),
    expires_at_unix: asInt(value.expires_at_unix),
    session_id: asString(value.session_id ?? value.sessionId),
    guest: value.guest === true,
    is_new_user: value.is_new_user === true,
    profile: {
      display_name: asString(profile.display_name ?? profile.displayName),
      avatar_url: asString(profile.avatar_url ?? profile.avatarUrl),
      avatar_media_id: asString(profile.avatar_media_id ?? profile.avatarMediaId),
      status: asString(profile.status),
      ...profile,
    },
  };
}

interface WireContent {
  text?: string;
  media?: unknown[];
  replyTo?: number;
  reply_to?: number;
  mentions?: unknown[];
}

function extractContent(value: unknown): WireContent {
  if (isRecord(value)) {
    let content = value;
    if (typeof value.content === 'string') {
      try {
        content = JSON.parse(value.content) as Record<string, unknown>;
      } catch {
        content = { text: value.content };
      }
    } else if (isRecord(value.content)) {
      content = { ...value.content, ...value };
    }
    return {
      text: asString(content.text ?? content.payload_text),
      media: asArray(content.media),
      replyTo: asInt(content.replyTo ?? content.reply_to),
      mentions: asArray(content.mentions),
    };
  }
  if (typeof value === 'string') {
    try {
      return extractContent(JSON.parse(value) as unknown);
    } catch {
      return { text: value };
    }
  }
  return {};
}

function parseAttachments(values: unknown[]): Attachment[] {
  return values.map((item) => {
    if (!isRecord(item)) return { kind: 'file' };
    const mime = asString(item.mime);
    const rawKind = asString(item.mtype ?? item.kind ?? item.type).toLowerCase();
    let kind: Attachment['kind'] = 'file';
    if (mime.startsWith('image/') || rawKind.includes('image') || rawKind === '0') kind = 'image';
    else if (mime.startsWith('audio/') || rawKind.includes('audio') || rawKind === '1') kind = 'audio';
    else if (mime.startsWith('video/') || rawKind.includes('video') || rawKind === '2') kind = 'video';
    return {
      id: asString(item.id) || undefined,
      media_id: asString(item.media_id ?? item.mediaId) || undefined,
      mediaId: asString(item.mediaId ?? item.media_id) || undefined,
      name: asString(item.name),
      mime,
      type: mime,
      size: asInt(item.size),
      width: asInt(item.width) || undefined,
      height: asInt(item.height) || undefined,
      waveform: Array.isArray(item.waveform) ? item.waveform as number[] : undefined,
      kind,
    };
  });
}

export function parseMessage(value: unknown, convId: number, fallbackUserId = 0): ChatMessage {
  const record = isRecord(value) ? value : {};
  const content = extractContent(record.payload ?? record.content ?? record);
  const id = asString(record.id ?? record.msg_id ?? record.msgId ?? record.client_msg_id ?? record.clientMsgId);
  const createdAt = asInt(record.created_at ?? record.createdAt ?? record.sent_at ?? record.sentAt ?? Date.now());
  return {
    id,
    seq: asInt(record.seq),
    convId: asInt(record.conv_id ?? record.convId) || convId,
    senderId: asInt(record.sender_id ?? record.senderId ?? record.from_user_id ?? fallbackUserId),
    text: content.text || '',
    createdAt: createdAt > 1e12 ? createdAt : createdAt * 1000,
    status: 'sent',
    replyTo: content.replyTo ? String(content.replyTo) : undefined,
    mentions: content.mentions?.map((n) => asInt(n)).filter(Boolean),
    media: parseAttachments(content.media || []),
    edited: record.edited === true,
    deleted: record.deleted === true || record.status === 'deleted',
    recalled: record.recalled === true || record.status === 'recalled',
  };
}

export function parseConversation(value: unknown): Conversation | null {
  if (!isRecord(value)) return null;
  const id = asInt(value.id ?? value.conv_id ?? value.convId);
  if (!id) return null;
  const rawType = asString(value.type ?? value.conv_type ?? value.convType).toLowerCase();
  const type = rawType === 'group' || rawType === '2' || asInt(value.conv_type ?? value.convType) === 2 ? 'group' : 'direct';
  return {
    id,
    title: asString(value.title ?? value.name) || `会话 ${id}`,
    type,
    pinned: value.pinned === true || value.is_pinned === true,
    muted: value.muted === true || value.is_muted === true,
    unread: asInt(value.unread ?? value.unread_count ?? value.unreadCount),
    lastSeq: asInt(value.last_seq ?? value.lastSeq ?? value.seq),
    lastText: asString(value.last_text ?? value.lastText ?? value.preview),
    lastAt: asInt(value.last_at ?? value.lastAt ?? value.updated_at ?? value.updatedAt),
    memberIds: asArray(value.member_ids ?? value.members ?? value.memberIds)
      .map((member) => (isRecord(member) ? asInt(member.user_id ?? member.userId) : asInt(member)))
      .filter(Boolean),
    lastReadSeq: asInt(value.last_read_seq ?? value.lastReadSeq),
  };
}
