/** WebRTC 1v1 通话状态机（最小可用：发起/接听/拒绝/挂断/静音/免提）。 */
import { K } from '../protocol/kinds';
import type { Frame } from '../protocol/frame';
import type { WsRequests } from './requests';
import { isRecord } from '../protocol/frame';

export type CallState = 'idle' | 'outgoing' | 'incoming' | 'active' | 'ended';

export interface CallInfo {
  state: CallState;
  convId: number;
  peerId: number;
  peerName: string;
  muted: boolean;
  speaker: boolean;
}

type Listener = (call: CallInfo) => void;

const initial: CallInfo = { state: 'idle', convId: 0, peerId: 0, peerName: '', muted: false, speaker: false };

export class CallSession {
  private info: CallInfo = initial;
  private listeners = new Set<Listener>();
  private pc: RTCPeerConnection | null = null;
  private local: MediaStream | null = null;
  private remote: MediaStream | null = null;
  private pendingOffer: unknown = null;
  private candidates: unknown[] = [];
  private requests: WsRequests | null = null;
  private callEnabled = true;

  onRemoteStream(listener: (stream: MediaStream | null) => void): () => void {
    this.remoteListeners.add(listener);
    return () => this.remoteListeners.delete(listener);
  }
  private remoteListeners = new Set<(s: MediaStream | null) => void>();

  constructor() {}

  configure(requests: WsRequests, callEnabled: boolean): void {
    this.requests = requests;
    this.callEnabled = callEnabled;
  }

  get enabled(): boolean { return this.callEnabled; }
  get call(): CallInfo { return this.info; }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    listener(this.info);
    return () => this.listeners.delete(listener);
  }

  private emit(patch: Partial<CallInfo>): void {
    this.info = { ...this.info, ...patch };
    for (const listener of this.listeners) listener(this.info);
  }

  private async send(kind: number, convId: number, target: number, payload: unknown): Promise<void> {
    if (!this.requests) return;
    try { await this.requests.request(kind, { ...(payload as Record<string, unknown>), target }, 8000, convId); }
    catch { /* 通话信令失败静默（对端不在线等），挂断流程自行收尾 */ }
  }

  private async ensurePeerConnection(convId: number): Promise<RTCPeerConnection> {
    if (this.pc) return this.pc;
    const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
    pc.onicecandidate = (event) => {
      if (event.candidate) void this.send(K.CALL_CANDIDATE, convId, this.info.peerId, { candidate: event.candidate.toJSON() });
    };
    pc.ontrack = (event) => {
      this.remote = event.streams[0] || null;
      for (const listener of this.remoteListeners) listener(this.remote);
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') this.hangup('连接已断开');
    };
    this.local = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
    for (const track of this.local.getTracks()) pc.addTrack(track, this.local);
    this.pc = pc;
    return pc;
  }

  private teardown(): void {
    for (const track of this.local?.getTracks() || []) track.stop();
    this.pc?.close();
    this.pc = null;
    this.local = null;
    this.remote = null;
    for (const listener of this.remoteListeners) listener(null);
    this.pendingOffer = null;
    this.candidates = [];
  }

  async start(convId: number, peerId: number, peerName: string): Promise<void> {
    if (this.info.state !== 'idle') return;
    this.emit({ state: 'outgoing', convId, peerId, peerName, muted: false, speaker: false });
    try {
      const pc = await this.ensurePeerConnection(convId);
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await this.send(K.CALL_OFFER, convId, peerId, { sdp: offer.sdp, type: offer.type });
    } catch {
      this.teardown();
      this.emit({ state: 'idle' });
      throw new Error('无法访问麦克风');
    }
  }

  async accept(): Promise<void> {
    if (this.info.state !== 'incoming' || !this.pc) return;
    await this.pc.setRemoteDescription(new RTCSessionDescription(this.pendingOffer as RTCSessionDescriptionInit));
    for (const candidate of this.candidates) {
      try { await this.pc.addIceCandidate(new RTCIceCandidate(candidate as RTCIceCandidateInit)); } catch { /* 忽略无效 candidate */ }
    }
    this.candidates = [];
    const answer = await this.pc.createAnswer();
    await this.pc.setLocalDescription(answer);
    await this.send(K.CALL_ANSWER, this.info.convId, this.info.peerId, { sdp: answer.sdp, type: answer.type });
    this.emit({ state: 'active' });
  }

  async reject(): Promise<void> {
    await this.send(K.CALL_BUSY, this.info.convId, this.info.peerId, { reason: 'busy' });
    this.teardown();
    this.emit({ state: 'idle' });
  }

  hangup(reason = '通话已结束'): void {
    const { convId, peerId, state } = this.info;
    if (state !== 'idle') void this.send(K.CALL_HANGUP, convId, peerId, { reason });
    this.teardown();
    this.emit({ state: 'idle' });
  }

  toggleMute(): void {
    const track = this.local?.getAudioTracks()[0];
    if (!track) return;
    track.enabled = this.info.muted;
    this.emit({ muted: !this.info.muted });
  }

  toggleSpeaker(): void {
    this.emit({ speaker: !this.info.speaker });
  }

  /** 帧入口：处理 350-354 下行。 */
  async handleFrame(frame: Frame): Promise<boolean> {
    const payload = isRecord(frame.payload) ? frame.payload : {};
    const peerId = typeof frame.user_id === 'number' ? frame.user_id : Number(payload.from ?? 0);
    switch (frame.kind) {
      case K.CALL_OFFER: {
        if (this.info.state !== 'idle') {
          await this.send(K.CALL_BUSY, frame.conv_id || 0, peerId, { reason: 'busy' });
          return true;
        }
        this.pendingOffer = { type: 'offer', sdp: payload.sdp };
        this.emit({ state: 'incoming', convId: frame.conv_id || 0, peerId, peerName: String(payload.name || peerId), muted: false, speaker: false });
        await this.ensurePeerConnection(frame.conv_id || 0);
        return true;
      }
      case K.CALL_ANSWER: {
        if (!this.pc) return true;
        await this.pc.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: typeof payload.sdp === 'string' ? payload.sdp : undefined }));
        for (const candidate of this.candidates) {
          try { await this.pc.addIceCandidate(new RTCIceCandidate(candidate as RTCIceCandidateInit)); } catch { /* 忽略 */ }
        }
        this.candidates = [];
        this.emit({ state: 'active' });
        return true;
      }
      case K.CALL_CANDIDATE: {
        if (!this.pc) return true;
        if (!this.pc.remoteDescription) this.candidates.push(payload.candidate ?? payload);
        else { try { await this.pc.addIceCandidate(new RTCIceCandidate(payload.candidate ?? payload)); } catch { /* 忽略 */ } }
        return true;
      }
      case K.CALL_BUSY: {
        this.teardown();
        this.emit({ state: 'idle' });
        window.dispatchEvent(new CustomEvent('ufot:toast', { detail: { message: '对方正忙，未能接通' } }));
        return true;
      }
      case K.CALL_HANGUP: {
        this.teardown();
        this.emit({ state: 'idle' });
        window.dispatchEvent(new CustomEvent('ufot:toast', { detail: { message: '对方已挂断' } }));
        return true;
      }
      default:
        return false;
    }
  }
}

export const callSession = new CallSession();
