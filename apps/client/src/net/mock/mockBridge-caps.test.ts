/**
 * §D-3 单测：mockBridge 不上报客户端能力 —— 不发 CLIENT_CAPS(18) 帧时视为
 * 旧客户端，保持现有 mock 投递行为（bootstrap 下发 + inline 逐条 MSG_NEW）。
 */
import { describe, expect, it } from 'vitest';
import { MockBridge } from './mockBridge';
import { KINDS } from '../protocol/kinds';
import type { Envelope } from '../proto/codec';

describe('mockBridge 能力协商（§D-3 不上报 = 旧客户端）', () => {
  it('start/bootstrap 全程不发 CLIENT_CAPS(18) 能力帧', async () => {
    const bridge = new MockBridge({ connectDelayMs: 0, persist: false });
    const frames: Envelope[] = [];
    bridge.onFrame((env) => frames.push(env));
    bridge.start();
    await new Promise((resolve) => setTimeout(resolve, 30));
    bridge.stop();
    expect(frames.some((env) => env.kind === KINDS.CLIENT_CAPS)).toBe(false);
    // 旧客户端语义保持：仍走 inline 下发（bootstrap CONV_PAYLOAD / FEATURE_FLAGS 等）
    expect(frames.length).toBeGreaterThan(0);
  });

  it('收到 CLIENT_CAPS 上行不崩溃、静默吞掉（兼容真实链路形态）', async () => {
    const bridge = new MockBridge({ connectDelayMs: 0, persist: false });
    bridge.start();
    await new Promise((resolve) => setTimeout(resolve, 10));
    const ok = bridge.send({
      kind: KINDS.CLIENT_CAPS,
      id: 1,
      ackId: 0,
      version: 1,
      requestId: 'client-caps',
      flags: 0,
      sentAt: Date.now(),
      payload: new TextEncoder().encode(JSON.stringify({ pull_capable: true })),
    });
    expect(ok).toBe(true);
    bridge.stop();
  });
});
