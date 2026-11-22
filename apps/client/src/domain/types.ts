export interface UserProfile {
  display_name?: string;
  avatar_url?: string;
  avatar_media_id?: string;
  status?: string;
  [key: string]: unknown;
}

export interface Session {
  user_id: number;
  device_id: string;
  access_token: string;
  token?: string;
  refresh_token: string;
  expires_at: number;
  expires_at_unix?: number;
  session_id: string;
  guest: boolean;
  is_new_user?: boolean;
  profile: UserProfile;
}

export type MessageStatus = 'sending' | 'sent' | 'delivered' | 'read' | 'failed';

export interface Attachment {
  id?: string;
  media_id?: string;
  mediaId?: string;
  name?: string;
  mime?: string;
  type?: string;
  size?: number;
  url?: string;
  width?: number;
  height?: number;
  waveform?: number[] | string;
  kind?: 'image' | 'audio' | 'video' | 'file';
}

export interface ChatMessage {
  id: string;
  seq: number;
  convId: number;
  senderId: number;
  text: string;
  createdAt: number;
  status: MessageStatus;
  replyTo?: string;
  mentions?: number[];
  media: Attachment[];
  edited?: boolean;
  deleted?: boolean;
  recalled?: boolean;
  pending?: boolean;
  error?: string;
}

export interface Conversation {
  id: number;
  title: string;
  type: 'direct' | 'group';
  pinned: boolean;
  muted: boolean;
  unread: number;
  lastSeq: number;
  lastText: string;
  lastAt: number;
  memberIds: number[];
  lastReadSeq: number;
}

export type ConnectionState = 'offline' | 'connecting' | 'online' | 'reconnecting' | 'banned';

export interface FeatureFlags {
  [key: string]: string | boolean;
}
