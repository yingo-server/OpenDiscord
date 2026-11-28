/**
 * WebSocket 帧编解码。
 *
 * 后端事实（docs/ws-protocol.md §2/§3）：
 * - 文本帧 = JSON，外层 envelope 字段为 snake_case；
 * - payload 在 JSON 线格式里是 **base64**（protobuf bytes 的 JSON 编码）；
 * - id 必须是数字（字符串 id 会被服务端判为坏帧并断连）；
 * - conv_id 与 user_id 互斥，同设直接判坏帧。
 */

export interface WireEnvelope {
  id?: number;
  ack_id?: number;
  kind: number;
  version?: number;
  request_id?: string;
  flags?: number;
  sent_at?: number;
  conv_id?: number;
  user_id?: number;
  payload?: string;
}

export interface DecodedFrame {
  id: number;
  ackId: number;
  kind: number;
  requestId: string;
  convId: number;
  userId: number;
  /** payload 解出的 JSON（服务端对 protobuf 负载可能解不出 JSON，此时为 null） */
  payload: Record<string, unknown> | null;
  /** 原始 base64 负载，供无法解析的场景兜底 */
  rawPayload: string | null;
}

export const WS_MAX_FRAME_BYTES = 64 * 1024;

/** UTF-8 安全的 base64 编码（btoa 只接受 latin1）。 */
export function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

export function base64ToBytes(base64: string): Uint8Array {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

export function encodePayload(payload: unknown): string {
  const json = JSON.stringify(payload ?? {});
  return bytesToBase64(new TextEncoder().encode(json));
}

export function decodePayload(base64: string | undefined): {
  payload: Record<string, unknown> | null;
  raw: string | null;
} {
  if (!base64) return { payload: null, raw: null };
  try {
    const text = new TextDecoder().decode(base64ToBytes(base64));
    const parsed = JSON.parse(text) as unknown;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return { payload: parsed as Record<string, unknown>, raw: base64 };
    }
    return { payload: null, raw: base64 };
  } catch {
    // 服务端 PONG 等帧可能带 protobuf 负载，解不出 JSON 属正常。
    return { payload: null, raw: base64 };
  }
}

export interface EncodeOptions {
  kind: number;
  id?: number;
  ackId?: number;
  requestId?: string;
  convId?: number;
  userId?: number;
  payload?: unknown;
  sentAt?: number;
}

/** 组装并序列化一帧（文本帧）。 */
export function encodeFrame(options: EncodeOptions): string {
  const envelope: WireEnvelope = { kind: options.kind };
  if (options.id !== undefined) envelope.id = options.id;
  if (options.ackId !== undefined) envelope.ack_id = options.ackId;
  if (options.requestId) envelope.request_id = options.requestId;
  if (options.convId !== undefined) envelope.conv_id = options.convId;
  if (options.userId !== undefined) envelope.user_id = options.userId;
  envelope.sent_at = options.sentAt ?? Date.now();
  if (options.payload !== undefined) envelope.payload = encodePayload(options.payload);
  return JSON.stringify(envelope);
}

/** 解析服务端下行帧。 */
export function decodeFrame(text: string): DecodedFrame | null {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return null;
  }
  if (!raw || typeof raw !== 'object') return null;
  const rec = raw as Record<string, unknown>;
  const kind = typeof rec.kind === 'number' ? rec.kind : 0;
  if (!kind) return null;
  const { payload, raw: rawPayload } = decodePayload(
    typeof rec.payload === 'string' ? rec.payload : undefined,
  );
  return {
    id: typeof rec.id === 'number' ? rec.id : 0,
    ackId: typeof rec.ack_id === 'number' ? rec.ack_id : 0,
    kind,
    requestId: typeof rec.request_id === 'string' ? rec.request_id : '',
    convId: typeof rec.conv_id === 'number' ? rec.conv_id : 0,
    userId: typeof rec.user_id === 'number' ? rec.user_id : 0,
    payload,
    rawPayload,
  };
}

/** 估算帧字节长度（超 64KB 服务端会 ERROR 4000 后以 1003 关闭）。 */
export function frameByteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}
