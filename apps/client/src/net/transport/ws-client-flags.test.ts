/**
 * 连接建立后主动拉取 feature flags（P0-4）：
 * 客户端此前只在 handler 注册里出现 FEATURE_FLAGS，从不发请求 → flags 恒空
 * → UI 能力开关恒真。ws-client 在 open 后必须主动发一帧 kind 12。
 */
import { describe, expect, it } from 'vitest';
import { WsClient } from './ws-client';
import { decodeEnvelope } from '../proto/codec';
import { KINDS } from '../protocol/kinds';
import type { SocketFactory, WsSocket } from './ws-connection';

class RecordingSocket implements WsSocket {
  readyState = 1; // OPEN
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number; reason: string; wasClean: boolean }) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  readonly sent: Array<string | ArrayBuffer | Uint8Array> = [];
  send(data: string | ArrayBuffer | Uint8Array): void {
    this.sent.push(data);
  }
  close(): void {}
}

function makeClient() {
  const sockets: RecordingSocket[] = [];
  const factory: SocketFactory = () => {
    const socket = new RecordingSocket();
    sockets.push(socket);
    return socket;
  };
  const client = new WsClient({
    url: 'ws://api.example.com/ws',
    onFrame: () => undefined,
    socketFactory: factory,
    heartbeatMs: 60_000,
  });
  return { client, sockets };
}

function kindsOf(ssocket: RecordingSocket): number[] {
  return ssocket.sent
    .filter((frame): frame is string => typeof frame === 'string')
    .map((frame) => decodeEnvelope(frame).kind);
}

describe('WsClient 连接建立后请求 feature flags（P0-4）', () => {
  it('open 后发送一帧 kind 12（FEATURE_FLAGS 请求）', () => {
    const { client, sockets } = makeClient();
    client.start();
    const socket = sockets[0];
    socket.onopen?.(); // 触发握手完成
    expect(kindsOf(socket)).toContain(KINDS.FEATURE_FLAGS);
    client.stop();
  });

  it('重连（再次 open）后仍会重新请求 flags', () => {
    const { client, sockets } = makeClient();
    client.start();
    sockets[0].onopen?.();
    const before = kindsOf(sockets[0]).filter((kind) => kind === KINDS.FEATURE_FLAGS).length;
    // 模拟重连：再次 open
    sockets[0].onopen?.();
    const after = kindsOf(sockets[0]).filter((kind) => kind === KINDS.FEATURE_FLAGS).length;
    expect(before).toBe(1);
    expect(after).toBe(2);
    client.stop();
  });
});

/**
 * 旧客户端能力协商（lts-1.4-acceptance §D，转生产阻断项）：
 * 连接 open 后必须立即上报 CLIENT_CAPS(18) {"pull_capable":true}，且每次
 * open（含重连）恰好一帧 —— 服务端按连接注册能力，重连必须重新登记。
 */
function capsOf(socket: RecordingSocket): Array<{ kind: number; payload: string }> {
  return socket.sent
    .filter((frame): frame is string => typeof frame === 'string')
    .map((frame) => decodeEnvelope(frame))
    .filter((env) => env.kind === KINDS.CLIENT_CAPS)
    .map((env) => ({ kind: env.kind, payload: new TextDecoder().decode(env.payload) }));
}

describe('WsClient 能力上报（§D CLIENT_CAPS / pull_capable）', () => {
  it('open 后立即上报一帧 CLIENT_CAPS(18)，payload {"pull_capable":true}', () => {
    const { client, sockets } = makeClient();
    client.start();
    const socket = sockets[0];
    socket.onopen?.();
    const caps = capsOf(socket);
    expect(caps).toHaveLength(1);
    expect(JSON.parse(caps[0].payload)).toEqual({ pull_capable: true });
    client.stop();
  });

  it('同一次连接 open 只发一帧（不重复上报）', () => {
    const { client, sockets } = makeClient();
    client.start();
    const socket = sockets[0];
    socket.onopen?.();
    // 心跳 / 其他活动不额外触发能力帧
    expect(capsOf(socket)).toHaveLength(1);
    client.stop();
  });

  it('重连后重新上报（能力随连接生命周期重新登记）', () => {
    const { client, sockets } = makeClient();
    client.start();
    sockets[0].onopen?.();
    expect(capsOf(sockets[0])).toHaveLength(1);
    // 模拟重连：再次 open
    sockets[0].onopen?.();
    expect(capsOf(sockets[0])).toHaveLength(2);
    client.stop();
  });
});
