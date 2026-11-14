/**
 * WS 连接状态机（与纯传输 ws-connection 分离）：
 *
 *    start() ──► connecting ──open──► online
 *                    │                  │
 *                    │ close            │ close / 心跳超时 / 浏览器 offline
 *                    ▼                  ▼
 *                 (退避重试) ◄──── reconnecting ──(多次失败/离线)──► offline
 *
 * 职责边界：
 *  - 断线指数退避（backoff.ts，500ms→8s）
 *  - 重连成功后逐会话发送 RESUME(last_seq)，交给 sync 引擎补 gap
 *  - 25s 心跳 PING，心跳失败只静默重建连接，**不打断输入/UI**
 *  - 所有下行帧解码后原样交给 onFrame（由 dispatch 查表），本层不解释业务 kind
 */
import { backoffDelay, type BackoffOptions } from './backoff';
import { WsConnection, type SocketFactory } from './ws-connection';
import {
  createEnvelope,
  decodeEnvelope,
  decodeEnvelopeProto,
  encodeEnvelope,
  encodeEnvelopeProto,
  type Envelope,
} from '../proto/codec';
import { decodePong, encodePing, encodeReadHistory } from '../proto/payload';
import { KINDS } from '../protocol/kinds';
import type { ConnState, ConnStateMeta, NetClient, ResumeTicket, StateListener } from './types';

export interface WsClientOptions {
  url: string;
  /**
   * token 经 Sec-WebSocket-Protocol `["bearer", token]` 传给服务端
   * （acceptor.go:288 只认 Cookie 与该头），仅在无 window 的原生壳生效。
   * 浏览器默认 undefined → 同源 Cookie 鉴权（security-spec §2.3）。
   */
  token?: () => string | null;
  deviceId?: () => string;
  sessionId?: () => string;
  resumeTickets?: () => ResumeTicket[];
  onFrame: (env: Envelope) => void;
  onState?: (state: ConnState, meta: ConnStateMeta) => void;
  format?: 'json' | 'protobuf';
  heartbeatMs?: number;
  socketFactory?: SocketFactory;
  backoff?: BackoffOptions;
  now?: () => number;
}

export class WsClient implements NetClient {
  private conn: WsConnection | null = null;
  private readonly options: WsClientOptions;
  private readonly format: 'json' | 'protobuf';
  private readonly heartbeatMs: number;

  private _state: ConnState = 'offline';
  private _latencyMs = 0;
  private attempt = 0;
  private frameId = 0;
  private started = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private heartbeatTimer: ReturnType<typeof setInterval> | null = null;
  private lastPongAt = 0;
  private resumeProvider: (() => ResumeTicket[]) | null = null;

  private frameListeners = new Set<(env: Envelope) => void>();
  private stateListeners = new Set<StateListener>();
  private browserOffline = typeof navigator !== 'undefined' && navigator.onLine === false;

  constructor(options: WsClientOptions) {
    this.options = options;
    this.format = options.format ?? 'json';
    this.heartbeatMs = options.heartbeatMs ?? 25_000;
    if (options.resumeTickets) this.resumeProvider = options.resumeTickets;
  }

  get state(): ConnState {
    return this._state;
  }

  get latencyMs(): number {
    return this._latencyMs;
  }

  start(): void {
    if (this.started) return;
    this.started = true;
    this.attempt = 0;
    this.bindBrowserEvents();
    this.setState('connecting');
    this.open();
  }

  stop(): void {
    this.started = false;
    this.clearTimers();
    this.unbindBrowserEvents();
    const conn = this.conn;
    this.conn = null;
    conn?.close(1000, 'client stop');
    this.setState('offline', 'client stop');
  }

  send(env: Envelope): boolean {
    if (!this.conn || this._state !== 'online') return false;
    const frame: Envelope = { ...env, id: env.id || ++this.frameId };
    const data = this.format === 'protobuf' ? encodeEnvelopeProto(frame) : encodeEnvelope(frame);
    return this.conn.send(data);
  }

  onFrame(listener: (env: Envelope) => void): () => void {
    this.frameListeners.add(listener);
    return () => this.frameListeners.delete(listener);
  }

  onState(listener: StateListener): () => void {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  setResumeProvider(provider: () => ResumeTicket[]): void {
    this.resumeProvider = provider;
  }

  // -------------------------------------------------------------------------
  // 内部：连接 / 重连 / 心跳
  // -------------------------------------------------------------------------

  private open(): void {
    const conn = new WsConnection(this.buildUrl(), {
      factory: this.options.socketFactory,
      protocols: this.authProtocols(),
      binaryType: 'arraybuffer',
    });
    this.conn = conn;

    conn.onOpen(() => {
      this.attempt = 0;
      this.lastPongAt = this.now();
      this.setState('online');
      this.sendResumeFrames();
      this.requestFeatureFlags();
      this.reportClientCaps();
      this.startHeartbeat();
    });

    conn.onMessage((data) => {
      let env: Envelope;
      try {
        env = this.format === 'protobuf' ? decodeEnvelopeProto(data as ArrayBuffer) : decodeEnvelope(data as string);
      } catch {
        return; // 畸形帧丢弃：codec 是边界，不让脏数据进入 dispatch
      }
      if (env.kind === KINDS.PONG) {
        try {
          const pong = decodePong(env.payload);
          if (pong.clientMs > 0) this._latencyMs = Math.max(0, this.now() - pong.clientMs);
        } catch {
          /* payload 畸形不影响 pong 本身 */
        }
        this.lastPongAt = this.now();
        this.emitState();
      }
      this.frameListeners.forEach((fn) => fn(env));
    });

    conn.onClose((info) => {
      this.stopHeartbeat();
      if (!this.started) return;
      this.scheduleReconnect(info.reason || `close ${info.code}`);
    });

    conn.onError(() => {
      /* onerror 之后浏览器必定派发 close，统一在 onClose 处理避免双重重连 */
    });

    conn.connect();
  }

  private scheduleReconnect(reason: string): void {
    if (!this.started || this.reconnectTimer) return;
    if (this.browserOffline) {
      this.setState('offline', 'browser offline');
      return;
    }
    this.attempt += 1;
    this.setState('reconnecting', reason);
    const delay = backoffDelay(this.attempt - 1, this.options.backoff);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.started) return;
      if (this.browserOffline) {
        this.setState('offline', 'browser offline');
        return;
      }
      this.open();
    }, delay);
  }

  private sendResumeFrames(): void {
    const tickets = this.resumeProvider ? this.resumeProvider() : [];
    const sessionId = this.options.sessionId?.() ?? '';
    for (const ticket of tickets) {
      const env = createEnvelope({
        kind: KINDS.RESUME,
        convId: ticket.convId,
        requestId: `resume-${ticket.convId}-${ticket.lastSeq}`,
        payload: new TextEncoder().encode(
          JSON.stringify({ session_id: sessionId, conv_id: ticket.convId, last_seq: ticket.lastSeq }),
        ),
      });
      this.send(env);
    }
  }

  /**
   * 连接建立后主动拉取 feature flags（kind 12）。
   * 服务端 router.go 对 FEATURE_FLAGS 做本地应答：回当前进程 flag 快照。
   * 客户端不主动问就永远拿不到 flags（UI 能力开关恒空 → 判定恒真，P0-4）。
   */
  private requestFeatureFlags(): void {
    const env = createEnvelope({
      kind: KINDS.FEATURE_FLAGS,
      requestId: 'feature-flags',
    });
    this.send(env);
  }

  /**
   * 旧客户端能力协商（lts-1.4-acceptance §D，转生产阻断项）：
   * 连接 open 后立即上报 CLIENT_CAPS(18)，payload JSON {"pull_capable":true}。
   * 服务端 user 层 Router 据此在连接注册表记该能力；universal_pull_enabled
   * 为 true 时仅 pull_capable 连接走水位线，无能力连接保持 inline 逐成员推。
   * 服务端同时兼容 proto Hello.pull_capable（字段号 10）与该 JSON 上报帧
   * （assemble.go:327 / core3_test.go:75 的载荷形态），二者语义等价。
   * 每次连接 open（含重连）都上报：能力随连接注册表生命周期，重连必须重新登记。
   */
  private reportClientCaps(): void {
    const env = createEnvelope({
      kind: KINDS.CLIENT_CAPS,
      requestId: 'client-caps',
      payload: new TextEncoder().encode(JSON.stringify({ pull_capable: true })),
    });
    this.send(env);
  }

  /**
   * 上滑历史：发 920 (READ_HISTORY)。离线返回 false（loader 走超时路径）。
   * 服务端按 beforeSeq>0 走"更早"方向，按 seq 升序回 921。
   */
  requestHistory(convId: number, beforeSeq: number, limit: number): boolean {
    if (this._state !== 'online') return false;
    const env = createEnvelope({
      kind: KINDS.READ_HISTORY,
      convId,
      payload: encodeReadHistory({ convId, beforeSeq, limit }),
    });
    return this.send(env);
  }

  private startHeartbeat(): void {
    this.stopHeartbeat();
    this.heartbeatTimer = setInterval(() => {
      if (this._state !== 'online') return;
      if (this.now() - this.lastPongAt > this.heartbeatMs * 2.2) {
        // 静默重建：UI 无感（NetworkBar 由 close 后的 reconnecting 驱动）
        this.conn?.close(4000, 'heartbeat timeout');
        this.conn = null;
        this.stopHeartbeat();
        if (this.started) this.scheduleReconnect('heartbeat timeout');
        return;
      }
      const ping = createEnvelope({ kind: KINDS.PING, payload: encodePing(this.now()) });
      this.send(ping);
    }, this.heartbeatMs);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private clearTimers(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.stopHeartbeat();
  }

  /**
   * 鉴权（security-spec §2.3 红线：token 禁止出现在 URL query —— 会进日志/Referer）：
   *  - 浏览器：登录接口已种下 HttpOnly cookie `access_token`，WS 同源握手自动携带，
   *    URL 不带任何凭据；
   *  - 原生壳（token-store 有 token 但无 cookie）：token 走
   *    Sec-WebSocket-Protocol: `bearer, <token>`（acceptor.go:288 支持的格式）。
   */
  private buildUrl(): string {
    const deviceId = this.options.deviceId?.();
    try {
      const base = typeof location !== 'undefined' ? location.href : 'http://localhost/';
      const url = new URL(this.options.url, base);
      if (deviceId) url.searchParams.set('device_id', deviceId);
      return url.toString();
    } catch {
      return this.options.url;
    }
  }

  /**
   * 非 Cookie 环境（原生壳等无 window/document 的运行时）的 subprotocol
   * 凭据。浏览器必须为 undefined：浏览器 WS 若请求 subprotocol 而服务端
   * （gorilla Upgrader 未配置 Subprotocols）不回显，握手会被浏览器直接
   * 失败 —— 浏览器路径只依赖登录种下的同源 HttpOnly Cookie。
   */
  private authProtocols(): string[] | undefined {
    if (typeof window !== 'undefined') return undefined;
    const token = this.options.token?.();
    return token ? ['bearer', token] : undefined;
  }

  private bindBrowserEvents(): void {
    if (typeof window === 'undefined') return;
    window.addEventListener('offline', this.onBrowserOffline);
    window.addEventListener('online', this.onBrowserOnline);
  }

  private unbindBrowserEvents(): void {
    if (typeof window === 'undefined') return;
    window.removeEventListener('offline', this.onBrowserOffline);
    window.removeEventListener('online', this.onBrowserOnline);
  }

  private onBrowserOffline = (): void => {
    this.browserOffline = true;
    this.conn?.close(4001, 'browser offline');
    this.conn = null;
    this.clearTimers();
    this.setState('offline', 'browser offline');
  };

  private onBrowserOnline = (): void => {
    this.browserOffline = false;
    if (!this.started || this._state === 'online') return;
    this.clearTimers();
    this.attempt = 0;
    this.setState('connecting');
    this.open();
  };

  private setState(state: ConnState, reason?: string): void {
    const changed = this._state !== state;
    this._state = state;
    if (changed || reason) this.emitState(reason);
  }

  private emitState(reason?: string): void {
    const meta: ConnStateMeta = {
      attempt: this.attempt,
      reason,
      latencyMs: this._latencyMs,
      at: this.now(),
    };
    this.options.onState?.(this._state, meta);
    this.stateListeners.forEach((fn) => fn(this._state, meta));
  }

  private now(): number {
    return this.options.now ? this.options.now() : Date.now();
  }
}
