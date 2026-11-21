/**
 * UI 运行时服务单例：main.tsx 注入真实实例，组件只消费接口。
 * 依赖方向：ui → net（类型），不反向。
 */
import { createHttpAuthApi, type AuthApi, type AuthSession } from '../../net/auth/provider';
import { createEnvelope } from '../../net/proto/codec';
import { encodeMsgRecall, type Media } from '../../net/proto/payload';
import { KINDS } from '../../net/protocol/kinds';
import { useMessageStore } from '../../core/stores/message-store';
import type { ChatMessage } from '../../core/types';
import type { Outbox } from '../../net/sync/outbox';
import type { SyncCache } from '../../net/cache/idb';
import type { SyncEngine } from '../../net/sync/engine';
import type { NetClient } from '../../net/transport/types';

export interface AppServices {
  outbox: Outbox | null;
  engine: SyncEngine | null;
  cache: SyncCache | null;
  client: NetClient | null;
  authApi: AuthApi;
  authenticate(session: AuthSession): void;
  logout(): void;
}

const fallback: AppServices = {
  outbox: null,
  engine: null,
  cache: null,
  client: null,
  authApi: createHttpAuthApi(),
  authenticate: () => {
    console.warn('[services] authenticate called before bootstrap');
  },
  logout: () => {
    console.warn('[services] logout called before bootstrap');
  },
};

let current: AppServices = fallback;

export function setAppServices(next: Partial<AppServices>): void {
  current = { ...current, ...next };
}

export function getAppServices(): AppServices {
  return current;
}

export function retrySend(localId: string): void {
  current.outbox?.retry(localId);
}

export function discardSend(localId: string): void {
  current.outbox?.discard(localId);
}

/**
 * 撤回：上行 MSG_RECALL(108) {msg_id, conv_id}（服务端 onRecall 已实现），
 * 同时本地乐观置 tombstone——成功后服务端广播的 108 会幂等地再打一次。
 */
export function recallMessage(message: ChatMessage): boolean {
  if (!message.msgId || !message.convId) return false;
  const { client, engine } = current;
  if (!client) return false;
  const sent = client.send(
    createEnvelope({
      kind: KINDS.MSG_RECALL,
      convId: message.convId,
      payload: encodeMsgRecall({ msgId: message.msgId, convId: message.convId }),
    }),
  );
  if (!sent) return false;
  // 乐观：立即折叠气泡；广播 108 到达后 sync.applyTombstone 幂等重放。
  void engine?.applyTombstone(message.convId, message.msgId, 'recall');
  useMessageStore.getState().applyTombstone(
    message.convId,
    message.msgId,
    message.seq,
    'recall',
  );
  return true;
}

/**
 * 转发构造（纯函数，可单测）：
 *  - 文本消息 → "[转发] " + 原文；
 *  - 媒体消息 → media 数组原样携带（media_id 引用同一文件），text 置空；
 *  - 混合 → 前缀文本 + media 原样。
 * system / 已撤回消息不可转发（返回 null）。
 */
export function buildForwardContent(message: ChatMessage): {
  text?: string;
  media?: Media[];
} | null {
  if (message.deleted || message.type === 'system') return null;
  const text = typeof message.payload.text === 'string' ? message.payload.text : '';
  const media = Array.isArray(message.payload.media) ? message.payload.media : [];
  if (media.length > 0) {
    return { text: text ? `${FORWARD_PREFIX}${text}` : '', media };
  }
  if (text) return { text: `${FORWARD_PREFIX}${text}` };
  return null;
}

/** 转发到目标会话：复用 outbox 乐观上屏 + MSG_ACK 语义。 */
/**
 * 举报消息：上行 REPORT(453) {target_msg, reason, detail}（服务端 onReport
 * 已实现、落 sys_reports + 审计，msgdomain_v2 默认开启）。detail 留空——
 * 客户端不采集自由文本，服务端最多截断 512B，PII 红线只存 id 与枚举。
 */
export function reportMessage(message: ChatMessage): boolean {
  if (!message.msgId) return false;
  const { client } = current;
  if (!client) return false;
  const payload = new TextEncoder().encode(
    JSON.stringify({ target_msg: message.msgId, reason: 1, detail: '' }),
  );
  return client.send(
    createEnvelope({
      kind: KINDS.REPORT,
      convId: message.convId,
      payload,
    }),
  );
}

export async function forwardMessages(message: ChatMessage, targetConvId: number): Promise<boolean> {
  const content = buildForwardContent(message);
  const { outbox } = current;
  if (!content || !outbox) return false;
  await outbox.enqueue({
    convId: targetConvId,
    text: content.text || undefined,
    media: content.media,
    mentions: [],
  });
  return true;
}

export const FORWARD_PREFIX = '[转发] ';
