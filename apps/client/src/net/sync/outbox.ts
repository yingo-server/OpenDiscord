/**
 * 离线上行队列（outbox）。
 *
 * 语义：
 *  - 发送即乐观插入（localId），断网期间排入 localStorage 队列；
 *  - 恢复在线后按 FIFO 顺序重发，同一时刻只允许一条 in-flight；
 *  - 服务端以 client_msg_id 去重，因此重发是幂等的；
 *  - ACK 到达 → 用服务器 msg_id/seq 确认本地消息；超时/失败 → 红点 + 手动重试。
 *
 * outbox 队列本身很小（只有内容引用，不含二进制），持久化用 localStorage，
 * 保持 IndexedDB 严格三库（messages/convs/mediaUrls）不变。
 */
import { createEnvelope, newRequestId } from '../proto/codec';
import { encodeMessageSend, type Media, type MsgAckPayload } from '../proto/payload';
import { KINDS } from '../protocol/kinds';
import type { ConnState, NetClient } from '../transport/types';

export interface OutboxContent {
  clientMsgId: string;
  text?: string;
  media?: Media[];
  replyTo?: number;
  mentions?: number[];
}

export type OutboxStatus = 'queued' | 'sending' | 'failed';

export interface OutboxItem {
  localId: string;
  clientMsgId: string;
  /** 前端生成的 trace id（Envelop.request_id，framerw 提取为 TraceID 透传） */
  traceId: string;
  convId: number;
  content: OutboxContent;
  status: OutboxStatus;
  attempts: number;
  createdAt: number;
  lastError?: string;
}

export interface OutboxSink {
  onOptimistic(item: OutboxItem): void;
  onSending(item: OutboxItem): void;
  onSent(item: OutboxItem, ack: { msgId: number; seq: number }): void;
  onFailed(item: OutboxItem, reason?: string): void;
  onRemoved(localId: string): void;
  onQueueChange(pending: number): void;
}

export interface OutboxPersistence {
  load(): Promise<OutboxItem[]>;
  save(items: OutboxItem[]): Promise<void>;
}

export interface OutboxOptions {
  /** 单条 ACK 超时（ms），超时标记 failed 并可重试 */
  ackTimeoutMs?: number;
  maxAttempts?: number;
  now?: () => number;
  newId?: () => string;
}

const STORAGE_KEY = 'opendiscord.outbox';

export function createLocalStoragePersistence(key = STORAGE_KEY): OutboxPersistence {
  const available = typeof localStorage !== 'undefined';
  return {
    async load() {
      if (!available) return [];
      try {
        const raw = localStorage.getItem(key);
        if (!raw) return [];
        const parsed = JSON.parse(raw) as unknown;
        return Array.isArray(parsed) ? (parsed as OutboxItem[]) : [];
      } catch {
        return [];
      }
    },
    async save(items) {
      if (!available) return;
      try {
        localStorage.setItem(key, JSON.stringify(items));
      } catch {
        /* 配额满：队列仍在内存，不阻塞发送 */
      }
    },
  };
}

export function createMemoryPersistence(initial: OutboxItem[] = []): OutboxPersistence {
  let snapshot = [...initial];
  return {
    async load() {
      return [...snapshot];
    },
    async save(items) {
      snapshot = [...items];
    },
  };
}

export class Outbox {
  private readonly sink: OutboxSink;
  private readonly persistence: OutboxPersistence;
  private readonly ackTimeoutMs: number;
  private readonly maxAttempts: number;
  private readonly now: () => number;
  private readonly newId: () => string;

  private client: NetClient | null = null;
  private items: OutboxItem[] = [];
  private inFlight: string | null = null;
  private ackTimer: ReturnType<typeof setTimeout> | null = null;
  private flushing = false;

  constructor(
    sink: OutboxSink,
    persistence: OutboxPersistence = createLocalStoragePersistence(),
    options: OutboxOptions = {},
  ) {
    this.sink = sink;
    this.persistence = persistence;
    this.ackTimeoutMs = options.ackTimeoutMs ?? 10_000;
    this.maxAttempts = options.maxAttempts ?? 5;
    this.now = options.now ?? Date.now;
    this.newId = options.newId ?? defaultId;
  }

  attach(client: NetClient): void {
    this.client = client;
  }

  get size(): number {
    return this.items.length;
  }

  pending(): OutboxItem[] {
    return this.items.map((item) => ({ ...item }));
  }

  /** 启动时恢复未发送队列，并重新插入乐观消息。 */
  async load(): Promise<void> {
    const stored = await this.persistence.load();
    // 旧队列条目无 traceId：补生成（存量兼容）
    this.items = stored.map((item) => ({
      ...item,
      traceId: item.traceId || newRequestId(),
      status: item.status === 'sending' ? 'queued' : item.status,
    }));
    for (const item of this.items) this.sink.onOptimistic(item);
    this.notify();
  }

  async enqueue(input: { convId: number; text?: string; media?: Media[]; replyTo?: number; mentions?: number[]; clientMsgId?: string }): Promise<OutboxItem> {
    const clientMsgId = input.clientMsgId ?? this.newId();
    const item: OutboxItem = {
      localId: `l_${clientMsgId}`,
      clientMsgId,
      traceId: newRequestId(),
      convId: input.convId,
      content: {
        clientMsgId,
        text: input.text,
        media: input.media,
        replyTo: input.replyTo,
        mentions: input.mentions,
      },
      status: 'queued',
      attempts: 0,
      createdAt: this.now(),
    };
    this.items.push(item);
    this.sink.onOptimistic(item);
    await this.persist();
    this.notify();
    void this.flush();
    return item;
  }

  /** MSG_ACK handler 调用；返回 false 表示 ACK 对应一条不存在的本地消息。 */
  ack(payload: MsgAckPayload): boolean {
    const item = this.items.find((candidate) => candidate.clientMsgId === payload.clientMsgId);
    if (!item) return false;
    this.removeItem(item.localId);
    this.clearTimer();
    this.inFlight = null;
    this.sink.onSent(item, { msgId: payload.msgId, seq: payload.seq });
    void this.persist();
    this.notify();
    void this.flush();
    return true;
  }

  /** 失败（超时/服务端拒绝）：保留条目，UI 显示红点。 */
  fail(clientMsgId: string, reason = 'send failed'): void {
    const item = this.items.find((candidate) => candidate.clientMsgId === clientMsgId);
    if (!item) return;
    item.status = 'failed';
    item.attempts += 1;
    item.lastError = reason;
    if (this.inFlight === item.localId) {
      this.inFlight = null;
      this.clearTimer();
    }
    this.sink.onFailed(item, reason);
    void this.persist();
    this.notify();
  }

  /**
   * 服务端拒绝（kind13 RATE_LIMITED / kind10 ERROR）到达时调用。
   * 这些帧不带 client_msg_id/request_id，无法精确配对——务实方案：把当前
   * in-flight 的条目标记 failed 并推进队列，避免 10s ack 超时挂起。
   * 未来服务端补上关联 id 后，可在调用方先做精确匹配，再回落到此方法。
   */
  failInFlight(reason = 'rejected'): void {
    if (!this.inFlight) return;
    const item = this.items.find((candidate) => candidate.localId === this.inFlight);
    if (!item) return;
    this.fail(item.clientMsgId, reason);
    void this.flush();
  }

  /** 用户点红点重试。 */
  retry(localId: string): void {
    const item = this.items.find((candidate) => candidate.localId === localId);
    if (!item) return;
    item.status = 'queued';
    item.lastError = undefined;
    void this.persist();
    this.notify();
    void this.flush();
  }

  /** 用户删除失败消息。 */
  discard(localId: string): void {
    if (!this.items.some((item) => item.localId === localId)) return;
    this.removeItem(localId);
    this.sink.onRemoved(localId);
    void this.persist();
    this.notify();
  }

  /** 连接状态变化：恢复在线立即按序重发；掉线把 in-flight 放回队列。 */
  handleState(state: ConnState): void {
    if (state === 'online') {
      void this.flush();
      return;
    }
    if (!this.inFlight) return;
    const item = this.items.find((candidate) => candidate.localId === this.inFlight);
    this.inFlight = null;
    this.clearTimer();
    if (item && item.status === 'sending') {
      item.status = 'queued';
      void this.persist();
      this.notify();
    }
  }

  async clear(): Promise<void> {
    this.items = [];
    this.inFlight = null;
    this.clearTimer();
    await this.persistence.save([]);
    this.notify();
  }

  /** 串行发送：一次只发出队首，ACK 后再发下一条，保证会话内顺序。 */
  async flush(): Promise<void> {
    if (this.flushing || this.inFlight) return;
    if (!this.client || this.client.state !== 'online') return;
    this.flushing = true;
    try {
      for (;;) {
        const next = this.items.find((item) => item.status === 'queued');
        if (!next) return;
        if (next.attempts >= this.maxAttempts) {
          next.status = 'failed';
          this.sink.onFailed(next, 'max attempts');
          continue;
        }
        this.inFlight = next.localId;
        next.status = 'sending';
        this.sink.onSending(next);
        await this.persist();
        this.notify();

        const env = createEnvelope({
          kind: KINDS.MSG_SEND,
          convId: next.convId,
          // request_id = 独立前端生成的 trace id（framerw 提取为 TraceID 透传），
          // 兜底 client_msg_id（ACK 关联语义不变：outbox.ack 按 client_msg_id 匹配）。
          requestId: next.traceId || next.clientMsgId,
          payload: encodeMessageSend({
            clientMsgId: next.clientMsgId,
            text: next.content.text,
            media: next.content.media,
            replyTo: next.content.replyTo,
            mentions: next.content.mentions,
          }),
        });
        const sent = this.client.send(env);
        if (!sent) {
          next.status = 'queued';
          this.inFlight = null;
          await this.persist();
          this.notify();
          return;
        }
        this.startAckTimer(next.clientMsgId);
        // 等待 ACK：由 ack()/fail() 推进队列，不在这里轮询
        return;
      }
    } finally {
      this.flushing = false;
    }
  }

  // -------------------------------------------------------------------------

  private removeItem(localId: string): void {
    this.items = this.items.filter((item) => item.localId !== localId);
  }

  private startAckTimer(clientMsgId: string): void {
    this.clearTimer();
    this.ackTimer = setTimeout(() => {
      this.ackTimer = null;
      this.fail(clientMsgId, 'ack timeout');
    }, this.ackTimeoutMs);
  }

  private clearTimer(): void {
    if (this.ackTimer) {
      clearTimeout(this.ackTimer);
      this.ackTimer = null;
    }
  }

  private async persist(): Promise<void> {
    try {
      await this.persistence.save(this.items);
    } catch {
      /* 持久化失败不阻塞发送 */
    }
  }

  private notify(): void {
    this.sink.onQueueChange(this.items.length);
  }
}

function defaultId(): string {
  const cryptoObj = typeof crypto !== 'undefined' ? crypto : undefined;
  if (cryptoObj && typeof cryptoObj.randomUUID === 'function') return cryptoObj.randomUUID();
  return `c_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}
