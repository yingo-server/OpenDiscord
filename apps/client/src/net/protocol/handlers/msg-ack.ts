/**
 * MSG_ACK(103) → outbox 确认（用服务器 msg_id/seq 替换本地乐观消息）。
 */
import { decodeMsgAck } from '../../proto/payload';
import type { Handler } from '../registry';

export const handleMsgAck: Handler = (env, ctx) => {
  let payload;
  try {
    payload = decodeMsgAck(env.payload);
  } catch (error) {
    console.warn('[msg-ack] bad payload', error);
    return;
  }
  if (!payload.clientMsgId) return;
  // status != 0 = 服务端落库失败/拒绝（pipe/ack.go）：不算成功，转失败红点而非 onSent。
  if (payload.status !== 0) {
    ctx.outbox.fail(payload.clientMsgId, `ack status ${payload.status}`);
    return;
  }
  const accepted = ctx.outbox.ack(payload);
  if (!accepted) {
    // 其他设备的发送回执：本机没有对应乐观消息，静默忽略。
    return;
  }
};
