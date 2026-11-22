import { encodeFrame, decodeFrame, type Frame } from '../protocol/frame';
import { retryDelay } from '../../domain/backoff';

export type ConnectionListener = (state: 'connecting' | 'online' | 'offline' | 'banned', detail?: string) => void;

export class WsClient {
  private ws: WebSocket | null = null;
  private timer: number | null = null;
  private heartbeat: number | null = null;
  private attempts = 0;
  private seq = 0;
  private stopped = false;
  private lastActivity = 0;
  private bannedUntil = 0;
  private readonly frameListeners = new Set<(frame: Frame) => void>();
  private readonly stateListeners = new Set<ConnectionListener>();
  private readonly queue: Frame[] = [];

  constructor(private readonly url: string) {}

  onFrame(listener: (frame: Frame) => void): () => void {
    this.frameListeners.add(listener);
    return () => this.frameListeners.delete(listener);
  }

  onState(listener: ConnectionListener): () => void {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  private emit(state: 'connecting' | 'online' | 'offline' | 'banned', detail?: string): void {
    for (const listener of this.stateListeners) listener(state, detail);
  }

  connect(): void {
    if (this.stopped || Date.now() < this.bannedUntil) return;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return;
    this.emit(this.attempts === 0 ? 'connecting' : 'offline');
    let ws: WebSocket;
    try {
      const device = sessionStorage.getItem('ufot.device');
      const sep = this.url.includes('?') ? '&' : '?';
      ws = new WebSocket(device ? `${this.url}${sep}device_id=${encodeURIComponent(device)}` : this.url);
    } catch {
      this.schedule();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.attempts = 0;
      this.lastActivity = Date.now();
      this.emit('online');
      this.send({ kind: 18, request_id: 'client-caps', payload: { pull_capable: true, pullCapable: true } });
      this.send({ kind: 12, request_id: 'feature-flags' });
      this.startHeartbeat();
      const queued = this.queue.splice(0);
      for (const frame of queued) this.write(frame);
    };
    ws.onmessage = (event) => {
      if (typeof event.data !== 'string') return;
      this.lastActivity = Date.now();
      const frame = decodeFrame(event.data as string);
      if (!frame) return;
      for (const listener of this.frameListeners) listener(frame);
    };
    ws.onerror = () => {
      // Reconnection is driven by close; technical errors are not shown to users.
    };
    ws.onclose = (event) => {
      this.ws = null;
      this.stopHeartbeat();
      if (this.stopped) return;
      if (event.code === 1008 || event.code === 4003) {
        this.bannedUntil = Date.now() + 60_000;
        this.emit('banned');
        return;
      }
      this.schedule();
    };
  }

  private startHeartbeat(): void {
    this.stopHeartbeat();
    this.heartbeat = window.setInterval(() => {
      if (this.ws?.readyState !== WebSocket.OPEN) return;
      if (Date.now() - this.lastActivity > 55_000) {
        this.ws.close(4000, 'heartbeat timeout');
        return;
      }
      this.send({ kind: 4, payload: { clientMs: Date.now(), client_ms: Date.now() } });
    }, 20_000);
  }

  private stopHeartbeat(): void {
    if (this.heartbeat !== null) clearInterval(this.heartbeat);
    this.heartbeat = null;
  }

  private schedule(): void {
    this.emit('offline');
    if (this.timer !== null) return;
    this.attempts += 1;
    this.timer = window.setTimeout(() => {
      this.timer = null;
      this.connect();
    }, retryDelay(this.attempts - 1));
  }

  send(frame: Omit<Frame, 'id' | 'sent_at'> & { id?: number }): number {
    const full: Frame = { ...frame, id: frame.id ?? ++this.seq, sent_at: Date.now() };
    if (this.ws?.readyState === WebSocket.OPEN) this.write(full);
    else this.queue.push(full);
    return full.id;
  }

  private write(frame: Frame): void {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(encodeFrame(frame));
  }

  close(): void {
    this.stopped = true;
    if (this.timer !== null) clearTimeout(this.timer);
    this.ws?.close(1000, 'client logout');
    this.ws = null;
  }

  resume(): void {
    this.stopped = false;
    this.bannedUntil = 0;
    this.connect();
  }
}
