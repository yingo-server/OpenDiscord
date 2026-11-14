/**
 * 前端侧 kind 常量 —— 与 server/internal/proto/kinds/kinds.go 一一对应。
 *
 * 这是前端唯一出现 kind 数值的地方；handler / renderer / dispatch 一律 import 常量，
 * 禁止裸数字（与后端 CI 规则一致）。号段表见 docs/architecture.md。
 */
export const KINDS = {
  // 0-99 系统层
  HELLO: 1,
  AUTH: 2,
  AUTH_RESULT: 3,
  PING: 4,
  PONG: 5,
  RESUME: 6,
  RESUME_RESULT: 7,
  SYNC_REQUEST: 8,
  SYNC_RESULT: 9,
  ERROR: 10,
  KICKED: 11,
  FEATURE_FLAGS: 12,
  RATE_LIMITED: 13,
  WATERMARK: 14, // 拉取模式：服务端 → 客户端会话最新 seq 通知（spec v1.2 §1.1；kinds.go 注释确认 14 空闲）

  // v3 消息总线（docs/v3-bus-design.md §4.1，0-99 系统层新增号段 15-19）
  MAILBOX_ACK: 15, // C→S 累积 ACK：{cursor} 确认已消费到 mailbox_seq=cursor
  MAILBOX_PAUSE: 16, // S→C 仅诊断：正常暂停靠 credit==0 停发，不发帧
  MAILBOX_RESUME: 17, // C→S 续传：{cursor}，语义 = RESUME(6) 的 mailbox 形态

  // 旧客户端能力协商（lts-1.4-acceptance §B，15-19 号段）
  CLIENT_CAPS: 18, // C→S 能力上报：payload JSON {"pull_capable":bool}；服务端据此决定是否走水位线

  // 100-199 消息层
  MSG_SEND: 101,
  MSG_NEW: 102,
  MSG_ACK: 103,
  MSG_READ: 104,
  MSG_DELIVERED: 105,
  MSG_EDIT: 106,
  MSG_DELETE: 107,
  MSG_RECALL: 108,
  MSG_FORWARD: 109,
  MSG_SYNC: 110,
  MSG_SEARCH: 111,

  // 200-299 会话层
  CONV_CREATE: 200,
  CONV_UPDATE: 201,
  CONV_PAYLOAD: 202,
  MEMBER_ADD: 210,
  MEMBER_REMOVE: 211,
  MEMBER_UPDATE: 212,
  ROLE_SET: 213,
  MUTE: 220,
  PIN: 221,
  TYPING: 230,
  DRAFT: 231,

  // 300-349 媒体层
  MEDIA_PRESIGN_REQUEST: 300,
  MEDIA_PRESIGN_RESULT: 301,
  MEDIA_READY: 302,
  MEDIA_EXPIRE: 303,
  MEDIA_THUMB: 304,

  // 350-399 通话预留
  CALL_OFFER: 350,
  CALL_ANSWER: 351,
  CALL_CANDIDATE: 352,
  CALL_BUSY: 353,
  CALL_HANGUP: 354,

  // 400-449 presence
  PRESENCE_UPDATE: 400,
  PRESENCE_SUBSCRIBE: 401,
  PRESENCE_UNSUBSCRIBE: 402,

  // 450-499 用户层
  PROFILE_UPDATE: 450,
  BLOCK: 451,
  UNBLOCK: 452,
  REPORT: 453,

  // 500-599 管理下行
  ADMIN_BROADCAST: 500,
  ANNOUNCE: 501,
  MAINTENANCE: 502,

  // 900-999 数据层（用户面代理到 930 会话数据）
  READ_HISTORY: 920,
  READ_HISTORY_RESULT: 921,
  CONV_DATA_REQUEST: 930,
  CONV_DATA_RESULT: 931,
} as const;

export type Kind = (typeof KINDS)[keyof typeof KINDS];

const NAMES: Record<number, string> = Object.fromEntries(
  Object.entries(KINDS).map(([name, value]) => [value, name]),
);

export function kindName(kind: number): string {
  return NAMES[kind] ?? `KIND_${kind}`;
}

export function kindLayer(kind: number): string {
  if (kind < 100) return 'system';
  if (kind < 200) return 'message';
  if (kind < 300) return 'conv';
  if (kind < 350) return 'media';
  if (kind < 400) return 'call';
  if (kind < 450) return 'presence';
  if (kind < 500) return 'user';
  if (kind < 600) return 'admin';
  if (kind < 900) return 'reserved';
  return 'internal';
}
