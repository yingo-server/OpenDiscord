/**
 * ERROR(10) → toast（不弹模态，不打断输入）；401 由 session port 决定是否登出。
 */
import { decodeError } from '../../proto/payload';
import type { Handler } from '../registry';

export const handleError: Handler = (env, ctx) => {
  let payload;
  try {
    payload = decodeError(env.payload);
  } catch {
    ctx.session.toast('toast.serverError');
    return;
  }
  ctx.session.toast('toast.serverError', { reason: payload.reason || String(payload.code) });
  // 同 rate-limited：ERROR 无关联 id，把 in-flight 出站消息立即标记失败，不挂 10s 超时。
  ctx.outbox.failInFlight(payload.reason || `error ${payload.code}`);
};
