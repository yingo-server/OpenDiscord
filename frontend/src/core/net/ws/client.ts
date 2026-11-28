/**
 * WebSocket 连接层：握手、心跳、指数退避重连、帧收发。
 *
 * 鉴权事实：
 * - 服务端握手只认 Cookie `access_token`；非浏览器客户端可用
 *   `Sec-WebSocket-Protocol: bearer, <token>`（本客户端走这条，适配跨源部署）。
 * - 禁止把 token 放 URL query。
 */

import { decodeFrame, encodeFrame, frameByteLength, WS_MAX_FRAME_BYTES } from './codec';
import { Kind } from './kinds';
import type { DecodedFrame } from './codec';
import { appendDebug } from '@/core/debug/db';

export type ConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'closed' | 'failed';

export interface WsClientOptions {
  url: string;
  /** access token（用于 Sec-WebSocket-Protocol 携带） */
  token?: string | null;
  /** 心跳间隔，默认 20s（对齐后端 60s 读超时） */
  heartbeatMs?: number;
  /** 退避基数，默认 500ms；上限 8s */
  backoffBaseMs?: number;
  backoffMaxMs?: number;
  onFrame: (frame: DecodedFrame) => void;
  onState?: (state: ConnectionState, detail?: string) => void;
  onClose?: (info: { code: number; reason: string; willReconnect: boolean }) => void;
}

const BACKOFF_JITTER = 0.2;

export class WsClient {
  private socket: WebSocket | null = null;
  private readonly options: {
    url: string;
    token: string | null;
    heartbeatMs: number;
    backoffBaseMs: number;
    backoffMaxMs: number;
    onFrame: (frame: DecodedFrame) => void;
    onState: (state: ConnectionState, detail?: string) => void;
    onClose: (info: { code: number; reason: string; willReconnect: boolean }) => void;
  };
  private nextId = 1;
  private heartbeatTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0;
  private manuallyClosed = false;
  /** 被服务端明确拒绝（401/403/4290）后的冷却截止时间 */
  private cooldownUntil = 0;
  /** 子协议握手是否已被服务端/浏览器拒绝（拒绝后不再重复尝试） */
  private protocolRejected = false;

  constructor(options: WsClientOptions) {
    this.options = {
      url: options.url,
      token: options.token ?? null,
      heartbeatMs: options.heartbeatMs ?? 20_000,
      backoffBaseMs: options.backoffBaseMs ?? 500,
      backoffMaxMs: options.backoffMaxMs ?? 8_000,
      onFrame: options.onFrame,
      onState: options.onState ?? (() => undefined),
      onClose: options.onClose ?? (() => undefined),
    };
  }

  get state(): ConnectionState {
    if (!this.socket) return 'idle';
    switch (this.socket.readyState) {
      case WebSocket.OPEN:
        return 'open';
      case WebSocket.CONNECTING:
        return 'connecting';
      default:
        return 'closed';
    }
  }

  /** 当前可用的帧 id（严格自增数字）。 */
  allocateId(): number {
    return this.nextId++;
  }

  connect(): void {
    this.manuallyClosed = false;
    if (this.socket && this.socket.readyState <= WebSocket.OPEN) return;
    if (Date.now() < this.cooldownUntil) {
      this.options.onState?.('reconnecting', '服务端限流冷却中');
      this.scheduleReconnect();
      return;
    }
    this.options.onState?.('connecting');

    // 握手两种鉴权方式，按优先级尝试：
    // 1) Sec-WebSocket-Protocol: bearer, <token>（跨源部署唯一可行方式，
    //    但服务端当前**不回显**所选子协议时浏览器会直接拒绝连接 → 记为失败原因）；
    // 2) 不带子协议，走 Cookie access_token（同源部署时可用）。
    const useProtocol = this.options.token && !this.protocolRejected;
    const protocols = useProtocol ? ['bearer', this.options.token as string] : [];
    let socket: WebSocket;
    try {
      socket = protocols.length > 0 ? new WebSocket(this.options.url, protocols) : new WebSocket(this.options.url);
    } catch (error) {
      this.options.onState?.('failed', error instanceof Error ? error.message : 'WebSocket 构造失败');
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    let opened = false;

    socket.onopen = () => {
      opened = true;
      this.attempt = 0;
      this.options.onState?.('open');
      this.startHeartbeat();
      // 上报能力：服务端据此改用水位线 + 拉取模式。
      this.send(Kind.CLIENT_CAPS, { pull_capable: true });
    };

    socket.onmessage = (event) => {
      if (typeof event.data !== 'string') return;
      const frame = decodeFrame(event.data);
      void appendDebug({
        kind: 'ws-recv', direction: 'in', url: this.options.url,
        body: event.data.length > 2000 ? `${event.data.slice(0, 2000)}…` : event.data,
        error: frame ? `kind=${frame.kind} req=${frame.requestId || '-'}` : 'decode-failed',
      });
      if (!frame) return;
      if (frame.kind === Kind.ERROR) {
        const code = Number(frame.payload?.code ?? 0);
        if (code === 4290) {
          // L4：被服务端断开，短时间内不重连。
          this.cooldownUntil = Date.now() + 60_000;
        }
      }
      this.options.onFrame(frame);
    };

    socket.onerror = () => {
      // 浏览器不会给出更多细节，真正的信息在 onclose。
    };

    socket.onclose = (event) => {
      this.stopHeartbeat();
      this.socket = null;
      if (!opened && useProtocol) {
        // 子协议握手被浏览器拒绝：多半是服务端没有回显所选子协议。
        this.protocolRejected = true;
        this.options.onState?.(
          'connecting',
          '子协议握手被拒（服务端未回显 Sec-WebSocket-Protocol），改用 Cookie 握手',
        );
        this.connect();
        return;
      }
      const willReconnect = !this.manuallyClosed && Date.now() >= this.cooldownUntil;
      this.options.onState?.(willReconnect ? 'reconnecting' : 'closed', event.reason);
      this.options.onClose?.({ code: event.code, reason: event.reason, willReconnect });
      if (willReconnect) this.scheduleReconnect();
    };
  }

  /** 主动断开且不再重连。 */
  close(): void {
    this.manuallyClosed = true;
    this.stopHeartbeat();
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.socket?.close(1000, 'client closed');
    this.socket = null;
    this.options.onState?.('closed');
  }

  /** 发送一帧；返回是否发送成功。 */
  send(
    kind: number,
    payload?: unknown,
    options: { convId?: number; userId?: number; requestId?: string; ackId?: number } = {},
  ): boolean {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) return false;
    const text = encodeFrame({
      kind,
      id: this.allocateId(),
      payload,
      convId: options.convId,
      userId: options.userId,
      requestId: options.requestId,
      ackId: options.ackId,
    });
    if (frameByteLength(text) > WS_MAX_FRAME_BYTES) {
      this.options.onState?.('failed', '帧超过 64KB 上限，已拦截');
      return false;
    }
    socket.send(text);
    void appendDebug({
      kind: 'ws-send', direction: 'out', url: this.options.url,
      body: text.length > 2000 ? `${text.slice(0, 2000)}…` : text,
      error: `kind=${kind} req=${options.requestId || '-'}`,
    });
    return true;
  }

  private startHeartbeat(): void {
    this.stopHeartbeat();
    this.heartbeatTimer = setInterval(() => {
      this.send(Kind.PING, { client_ms: Date.now() });
    }, this.options.heartbeatMs);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private scheduleReconnect(): void {
    if (this.manuallyClosed) return;
    if (this.reconnectTimer) return;
    const base = Math.min(this.options.backoffMaxMs, this.options.backoffBaseMs * 2 ** this.attempt);
    const jitter = base * BACKOFF_JITTER * (Math.random() * 2 - 1);
    const delay = Math.max(250, Math.round(base + jitter));
    this.attempt += 1;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }
}
