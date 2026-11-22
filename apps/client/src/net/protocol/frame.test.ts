import { describe, expect, it } from 'vitest';
import { decodeFrame, encodeFrame } from './frame';
import { K } from './kinds';

describe('frame codec', () => {
  it('round-trips a MSG_SEND frame with base64 payload', () => {
    const raw = encodeFrame({ id: 7, kind: K.MSG_SEND, conv_id: 42, request_id: 'c-1', payload: { text: 'hi', clientMsgId: 'c-1' } });
    const envelope = JSON.parse(raw) as Record<string, unknown>;
    expect(envelope.kind).toBe(101);
    expect(envelope.conv_id).toBe(42);
    expect(typeof envelope.payload).toBe('string');
    const frame = decodeFrame(raw);
    expect(frame?.id).toBe(7);
    expect(frame?.kind).toBe(K.MSG_SEND);
    expect(frame?.payload).toEqual({ text: 'hi', clientMsgId: 'c-1' });
  });

  it('ignores unknown kinds rather than throwing', () => {
    const raw = encodeFrame({ id: 1, kind: 98765 });
    expect(decodeFrame(raw)?.kind).toBe(98765);
  });

  it('rejects malformed frames', () => {
    expect(decodeFrame('not json')).toBeNull();
    expect(decodeFrame('{"kind":0}')).toBeNull();
    expect(decodeFrame('{"kind":101,"conv_id":1,"user_id":2}')).toBeNull();
  });
});
