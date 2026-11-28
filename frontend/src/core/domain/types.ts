/** 领域模型：与后端契约一一对应（字段名保持后端原样，避免二次映射出错）。 */

export interface Session {
  user_id: number;
  device_id: string;
  access_token: string;
  token?: string;
  refresh_token: string;
  /** 毫秒时间戳（实测为 ms，与源码注释相反） */
  expires_at: number;
  expires_at_unix: number;
  session_id: string;
  guest: boolean;
  is_new_user: boolean;
  profile?: { display_name?: string };
}

export type AuthMethod = 'phone' | 'email' | 'password' | 'guest';

export interface AuthMethods {
  phone: boolean;
  email: boolean;
  password: boolean;
  guest: boolean;
}

export interface MediaItem {
  mediaId: string;
  mtype: number;
  name?: string;
  size?: number;
  mime?: string;
  width?: number;
  height?: number;
  durationMs?: number;
}

export interface MessageContent {
  clientMsgId?: string;
  client_msg_id?: string;
  text?: string;
  media?: MediaItem[];
  replyTo?: number;
  reply_to?: number;
  mentions?: number[];
  forward_from?: { msgId?: number; msg_id?: number; convId?: number; conv_id?: number };
}

export type MessageStatus = 'pending' | 'sent' | 'delivered' | 'read' | 'failed';

export interface ChatMessage {
  /** 服务端消息 id；本地乐观消息用负数临时 id */
  msgId: number;
  convId: number;
  seq: number;
  senderId: number;
  text: string;
  media?: MediaItem[];
  replyTo?: number;
  mentions?: number[];
  sentAt: number;
  status: MessageStatus;
  /** 撤回/删除墓碑 */
  tombstone?: boolean;
  /** 转发来源 */
  forwardFrom?: { msgId: number; convId: number };
  clientMsgId?: string;
}

export interface ConvMember {
  userId: number;
  role: number;
  joinedAt: number;
  mutedUntil: number;
}

export interface Conversation {
  convId: number;
  title: string;
  convType: number;
  createdAt: number;
  members: ConvMember[];
  memberCount?: number;
  membersOmitted?: boolean;
  version?: number;
  lastSeq?: number;
  unread?: number;
  /** 客户端本地派生 */
  pinned?: boolean;
  muted?: boolean;
  lastMessage?: ChatMessage;
  lastActivity?: number;
}

export interface UserProfile {
  user_id: number;
  display_name?: string;
  avatar_media_id?: string;
  status?: number;
  created_at?: number;
}

export interface Paged<T> {
  items: T[];
  total: number;
}
