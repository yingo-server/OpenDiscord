/**
 * RATE_LIMITED(13) → 限流提示（就地 toast，不阻塞发送队列）。
 */
import { decodeRateLimited } from '../../proto/payload';
import type { Handler } from '../registry';

export const handleRateLimited: Handler = (env, ctx) => {
  let payload;
  try {
    payload = decodeRateLimited(env.payload);
  } catch {
    return;
  }
  ctx.session.rateLimited(payload.scope, payload.retryAfterMs);
  // 拒绝无法按 request_id 配对（服务端 kind13 不带关联 id）：fail 当前 in-flight 条目，
  // 避免 10s ack 超时挂起；flush 会推进下一条。未来服务端若回传关联 id，可优先精确匹配。
  ctx.outbox.failInFlight('rate limited');
};
