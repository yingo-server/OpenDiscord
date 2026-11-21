/**
 * handler 层回归测试：
 *  - receipt：批量 receipts 逐条应用（P1）；
 *  - member-update(212)：add/role 合入会话、remove 交由 202 快照（P1）；
 *  - call-offer：call_enabled 只认 "true"（P0-4）。
 */
import { describe, expect, it, vi } from 'vitest';
import { createEnvelope } from '../../proto/codec';
import { KINDS } from '../kinds';
import type { HandlerContext } from '../registry';
import { handleReceipt } from './receipt';
import { handleMemberUpdate } from './member-update';
import { handleCallOffer } from './call-offer';
import { handleMsgAck } from './msg-ack';
import { handleRateLimited } from './rate-limited';
import { handleError } from './error';
import { Outbox, createMemoryPersistence } from '../../sync/outbox';

const enc = (value: unknown): Uint8Array => new TextEncoder().encode(JSON.stringify(value));

function makeCtx(overrides: Partial<HandlerContext> = {}): HandlerContext {
  return {
    client: { send: vi.fn() } as unknown as HandlerContext['client'],
    sync: {} as HandlerContext['sync'],
    outbox: {} as HandlerContext['outbox'],
    session: {
      userId: 1,
      setFlags: vi.fn(),
      kicked: vi.fn(),
      rateLimited: vi.fn(),
      toast: vi.fn(),
      flag: vi.fn(),
      flags: vi.fn(() => ({})),
    },
    convs: { upsertFromWire: vi.fn(), setActive: vi.fn() },
    presence: { setStatus: vi.fn(), setTyping: vi.fn() },
    messages: { applyReceipt: vi.fn() },
    navigate: vi.fn(),
    toast: vi.fn(),
    ...overrides,
  };
}

describe('handleReceipt 批量 receipts（P1）', () => {
  it('遍历应用全部回执，不只第一条', () => {
    const ctx = makeCtx();
    const env = createEnvelope({
      kind: KINDS.MSG_READ,
      convId: 9,
      payload: enc({ receipts: [
        { msg_id: 10, user_id: 1, at: 100 },
        { msg_id: 11, user_id: 2, at: 200 },
      ] }),
    });
    handleReceipt(env, ctx);
    const apply = ctx.messages.applyReceipt as unknown as ReturnType<typeof vi.fn>;
    expect(apply).toHaveBeenCalledTimes(2);
    expect(apply.mock.calls[0][1]).toBe(10);
    expect(apply.mock.calls[1][1]).toBe(11);
    expect(apply.mock.calls[0][2]).toBe('read');
  });
});

describe('handleMemberUpdate(212)', () => {
  it('add：把成员合入会话（upsertFromWire）', () => {
    const ctx = makeCtx();
    const env = createEnvelope({
      kind: KINDS.MEMBER_UPDATE,
      convId: 5,
      payload: enc({ convId: 5, op: 'add', member: { userId: 42, role: 1, joinedAt: 1000 } }),
    });
    handleMemberUpdate(env, ctx);
    const upsert = ctx.convs.upsertFromWire as unknown as ReturnType<typeof vi.fn>;
    expect(upsert).toHaveBeenCalledTimes(1);
    const payload = upsert.mock.calls[0][0];
    expect(payload.convId).toBe(5);
    expect(payload.members[0]).toMatchObject({ userId: 42, role: 1, joinedAt: 1000 });
  });

  it('role：同样合入成员行', () => {
    const ctx = makeCtx();
    const env = createEnvelope({
      kind: KINDS.MEMBER_UPDATE,
      convId: 5,
      payload: enc({ convId: 5, op: 'role', member: { userId: 7, role: 2 } }),
    });
    handleMemberUpdate(env, ctx);
    expect(ctx.convs.upsertFromWire).toHaveBeenCalledTimes(1);
  });

  it('remove：本地不改成员（交由伴随的 202 快照收敛）', () => {
    const ctx = makeCtx();
    const env = createEnvelope({
      kind: KINDS.MEMBER_UPDATE,
      convId: 5,
      payload: enc({ convId: 5, op: 'remove', member: { userId: 7 } }),
    });
    handleMemberUpdate(env, ctx);
    expect(ctx.convs.upsertFromWire).not.toHaveBeenCalled();
  });
});

describe('handleCallOffer call_enabled 判定（P0-4）', () => {
  const offer = enc({ callId: 'c1', convId: 1, toUserId: 1, sdp: 'x', media: 'audio', fromUserId: 2 });

  it("flags={'call_enabled':'true'} 时不回 BUSY", () => {
    const ctx = makeCtx({ session: { ...makeCtx().session, flags: () => ({ call_enabled: 'true' }) } });
    handleCallOffer(createEnvelope({ kind: KINDS.CALL_OFFER, payload: offer }), ctx);
    expect(ctx.client.send).not.toHaveBeenCalled();
  });

  it("flags={'call_enabled':'false'} 时回 BUSY(unsupported)", () => {
    const ctx = makeCtx({ session: { ...makeCtx().session, flags: () => ({ call_enabled: 'false' }) } });
    handleCallOffer(createEnvelope({ kind: KINDS.CALL_OFFER, payload: offer }), ctx);
    expect(ctx.client.send).toHaveBeenCalledTimes(1);
  });

  it('flags 为空（拿不到）时保守回 BUSY', () => {
    const ctx = makeCtx(); // flags: () => ({})
    handleCallOffer(createEnvelope({ kind: KINDS.CALL_OFFER, payload: offer }), ctx);
    expect(ctx.client.send).toHaveBeenCalledTimes(1);
  });
});

// ---------------------------------------------------------------------------
// ACK 语义修复（V2-0 容量契约 · 客户端部分）
// ---------------------------------------------------------------------------

describe('handleMsgAck status 语义', () => {
  const ackEnv = (status: number) =>
    createEnvelope({
      kind: KINDS.MSG_ACK,
      convId: 5,
      payload: enc({ msgId: 100, seq: 7, clientMsgId: 'cm1', status }),
    });

  it('status=0：outbox.ack 成功路径', () => {
    const ctx = makeCtx({ outbox: { ack: vi.fn(() => true), fail: vi.fn() } as never });
    handleMsgAck(ackEnv(0), ctx);
    expect((ctx.outbox.ack as ReturnType<typeof vi.fn>)).toHaveBeenCalledTimes(1);
    expect((ctx.outbox.ack as ReturnType<typeof vi.fn>).mock.calls[0][0]).toMatchObject({
      msgId: 100,
      seq: 7,
      clientMsgId: 'cm1',
      status: 0,
    });
    expect((ctx.outbox.fail as ReturnType<typeof vi.fn>)).not.toHaveBeenCalled();
  });

  it('status!=0：转 fail(clientMsgId, reason)，不当成功', () => {
    const ctx = makeCtx({ outbox: { ack: vi.fn(), fail: vi.fn() } as never });
    handleMsgAck(ackEnv(1), ctx);
    expect((ctx.outbox.ack as ReturnType<typeof vi.fn>)).not.toHaveBeenCalled();
    expect((ctx.outbox.fail as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith('cm1', expect.stringContaining('1'));
  });
});

describe('handleRateLimited / handleError 拒绝配对', () => {
  it('rate-limited：failInFlight 被调用（不挂 10s ack 超时）', () => {
    const ctx = makeCtx({ outbox: { failInFlight: vi.fn() } as never });
    handleRateLimited(
      createEnvelope({ kind: KINDS.RATE_LIMITED, payload: enc({ scope: 'msg', retry_after_ms: 2000 }) }),
      ctx,
    );
    expect(ctx.session.rateLimited).toHaveBeenCalled();
    expect((ctx.outbox.failInFlight as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith('rate limited');
  });

  it('error(503 busy)：failInFlight 携带 reason', () => {
    const ctx = makeCtx({ outbox: { failInFlight: vi.fn() } as never });
    handleError(
      createEnvelope({ kind: KINDS.ERROR, payload: enc({ code: 503, reason: 'busy' }) }),
      ctx,
    );
    expect(ctx.session.toast).toHaveBeenCalled();
    expect((ctx.outbox.failInFlight as ReturnType<typeof vi.fn>)).toHaveBeenCalledWith('busy');
  });
});

describe('Outbox.failInFlight（真实 outbox）', () => {
  it('有 in-flight 时标记 failed 并推进 flush 下一条', async () => {
    const sent: string[] = [];
    const events: string[] = [];
    const client = {
      state: 'online' as const,
      send: (env: unknown) => {
        sent.push(JSON.stringify(env));
        return true;
      },
    };
    const outbox = new Outbox(
      {
        onOptimistic: () => undefined,
        onSending: () => undefined,
        onSent: () => undefined,
        onFailed: (item) => events.push(`failed:${item.clientMsgId}:${item.lastError}`),
        onRemoved: () => undefined,
        onQueueChange: () => undefined,
      },
      createMemoryPersistence(),
      { ackTimeoutMs: 10_000, now: () => 1, newId: (() => `c${sent.length}`) as () => string },
    );
    outbox.attach(client as unknown as Parameters<typeof outbox.attach>[0]);
    await outbox.enqueue({ convId: 1, text: 'a' });
    await outbox.enqueue({ convId: 1, text: 'b' });
    // 第一条已发出（in-flight），第二条 queued
    expect(sent.length).toBe(1);
    outbox.failInFlight('rate limited');
    const items = outbox.pending();
    expect(items.find((i) => i.status === 'failed')?.lastError).toBe('rate limited');
    // 队列推进：第二条变 in-flight 发出
    expect(sent.length).toBe(2);
    expect(outbox.pending().filter((i) => i.status === 'sending').length).toBe(1);
  });

  it('无 in-flight 时 no-op', () => {
    const outbox = new Outbox(
      {
        onOptimistic: () => undefined,
        onSending: () => undefined,
        onSent: () => undefined,
        onFailed: () => undefined,
        onRemoved: () => undefined,
        onQueueChange: () => undefined,
      },
      createMemoryPersistence(),
    );
    outbox.failInFlight('x');
    expect(outbox.pending().length).toBe(0);
  });

  it('status=0 ack 路径不回归：ack 后队列清空并推进', async () => {
    let sentCount = 0;
    const client = {
      state: 'online' as const,
      send: () => {
        sentCount += 1;
        return true;
      },
    };
    let confirmed = 0;
    const outbox = new Outbox(
      {
        onOptimistic: () => undefined,
        onSending: () => undefined,
        onSent: () => {
          confirmed += 1;
        },
        onFailed: () => undefined,
        onRemoved: () => undefined,
        onQueueChange: () => undefined,
      },
      createMemoryPersistence(),
      { ackTimeoutMs: 10_000, now: () => 1 },
    );
    outbox.attach(client as unknown as Parameters<typeof outbox.attach>[0]);
    const item = await outbox.enqueue({ convId: 1, text: 'hi' });
    expect(outbox.ack({ msgId: 9, seq: 1, clientMsgId: item.clientMsgId, status: 0 })).toBe(true);
    expect(confirmed).toBe(1);
    expect(outbox.size).toBe(0);
  });
});
