export interface Frame {
  id: number;
  ack_id?: number;
  kind: number;
  version?: number;
  request_id?: string;
  flags?: number;
  sent_at?: number;
  conv_id?: number;
  user_id?: number;
  payload?: unknown;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function b64encode(value: string): string {
  let binary = '';
  for (const byte of new TextEncoder().encode(value)) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function b64decode(value: string): string {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

export function encodeFrame(frame: Frame): string {
  const envelope: Record<string, unknown> = { kind: frame.kind };
  if (frame.id) envelope.id = frame.id;
  if (frame.ack_id) envelope.ack_id = frame.ack_id;
  if (frame.version) envelope.version = frame.version;
  if (frame.request_id) envelope.request_id = frame.request_id;
  if (frame.flags) envelope.flags = frame.flags;
  if (frame.sent_at) envelope.sent_at = frame.sent_at;
  if (frame.conv_id) envelope.conv_id = frame.conv_id;
  if (frame.user_id) envelope.user_id = frame.user_id;
  if (frame.payload !== undefined) envelope.payload = b64encode(JSON.stringify(frame.payload));
  return JSON.stringify(envelope);
}

export function decodeFrame(raw: string): Frame | null {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isRecord(value)) return null;
  const kind = value.kind;
  if (typeof kind !== 'number' || !Number.isInteger(kind) || kind <= 0) return null;
  if (value.conv_id && value.user_id) return null;
  const frame: Frame = { id: typeof value.id === 'number' ? value.id : 0, kind };
  if (typeof value.ack_id === 'number') frame.ack_id = value.ack_id;
  if (typeof value.version === 'number') frame.version = value.version;
  if (typeof value.request_id === 'string') frame.request_id = value.request_id;
  if (typeof value.flags === 'number') frame.flags = value.flags;
  if (typeof value.sent_at === 'number') frame.sent_at = value.sent_at;
  if (typeof value.conv_id === 'number') frame.conv_id = value.conv_id;
  if (typeof value.user_id === 'number') frame.user_id = value.user_id;
  if (typeof value.payload === 'string' && value.payload) {
    try {
      frame.payload = JSON.parse(b64decode(value.payload));
    } catch {
      frame.payload = value.payload;
    }
  }
  return frame;
}
