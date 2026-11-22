import { describe, expect, it } from 'vitest';
import { retryDelay } from './backoff';

describe('reconnect backoff', () => {
  it('respects server retry-after', () => {
    expect(retryDelay(5, 250)).toBe(250);
  });
  it('grows exponentially and caps at 8 seconds', () => {
    expect(retryDelay(0)).toBeGreaterThanOrEqual(500);
    expect(retryDelay(0)).toBeLessThan(800);
    expect(retryDelay(10)).toBe(8000);
  });
});
