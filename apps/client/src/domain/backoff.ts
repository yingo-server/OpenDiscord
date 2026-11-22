export function retryDelay(attempt: number, retryAfterMs?: number, max = 8000): number {
  if (retryAfterMs && retryAfterMs > 0) return Math.min(retryAfterMs, max);
  const base = Math.min(500 * 2 ** Math.max(0, attempt), max);
  return Math.min(base + Math.floor(Math.random() * Math.min(250, base / 2)), max);
}

export function backoffState(attempt: number): { attempt: number; delay: number } {
  return { attempt, delay: retryDelay(attempt) };
}
