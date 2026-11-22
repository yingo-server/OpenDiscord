/**
 * 通用 WS 请求-响应关联。
 * 契约 §3：请求帧带 id，响应回同一 id；失败回 kind 10 ERROR（同 id）。
 * 其他业务回包（941 等）也回同一 id，按帧头 id 关联。
 */
import type { Frame } from '../protocol/frame';
import { K } from '../protocol/kinds';
import { isRecord } from '../protocol/frame';

export interface WsPendingResult {
  ok: boolean;
  payload: unknown;
}

interface Waiter {
  resolve: (result: WsPendingResult) => void;
  reject: (error: Error) => void;
  timer: number;
}

export class WsRequests {
  private readonly waiters = new Map<number, Waiter>();
  private nextId = 1;

  constructor(private readonly send: (frame: Omit<Frame, 'id' | 'sent_at'> & { id?: number }) => number) {}

  request(kind: number, payload: unknown, timeoutMs = 10_000, convId?: number): Promise<WsPendingResult> {
    const id = this.nextId++;
    return new Promise<WsPendingResult>((resolve, reject) => {
      const timer = window.setTimeout(() => {
        this.waiters.delete(id);
        reject(new Error('timeout'));
      }, timeoutMs);
      this.waiters.set(id, { resolve, reject, timer });
      this.send({ kind, conv_id: convId, payload, id });
    });
  }

  /** 分发入口：ERROR / 与等待 id 匹配的回包走这里，返回 true 表示已消费。 */
  handle(frame: Frame): boolean {
    if (!frame.id || frame.kind === K.PONG) return false;
    const waiter = this.waiters.get(frame.id);
    if (!waiter) return false;
    this.waiters.delete(frame.id);
    clearTimeout(waiter.timer);
    const payload: unknown = frame.payload;
    if (frame.kind === K.ERROR) {
      const message = isRecord(frame.payload) && typeof frame.payload.message === 'string'
        ? frame.payload.message
        : '';
      waiter.reject(new Error(message || 'error'));
      return true;
    }
    const ok = isRecord(payload) ? payload.ok !== false : true;
    if (ok) waiter.resolve({ ok: true, payload });
    else waiter.reject(new Error(isRecord(payload) && typeof payload.error === 'string' ? payload.error : 'failed'));
    return true;
  }

  rejectAll(): void {
    for (const [, waiter] of this.waiters) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error('connection closed'));
    }
    this.waiters.clear();
  }
}

/** 解析 941 IDENTITY_RESULT 风格回包的 base64 body。 */
export function parseIdentityResult(payload: unknown): Record<string, unknown> | null {
  if (!isRecord(payload)) return null;
  const body = typeof payload.body === 'string' ? payload.body : '';
  if (!body) return null;
  try {
    const decoded = JSON.parse(atob(body)) as unknown;
    return isRecord(decoded) ? decoded : null;
  } catch {
    return null;
  }
}
