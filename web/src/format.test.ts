import { describe, expect, it } from 'vitest';

import { formatBytes } from './format';

describe('formatBytes', () => {
  it('formats the plan example and common sizes', () => {
    expect(formatBytes(8_400_000_000)).toBe('8.4 GB');
    expect(formatBytes(123_000_000)).toBe('123 MB');
    expect(formatBytes(45_600_000)).toBe('45.6 MB');
    expect(formatBytes(512_000)).toBe('512 KB');
    expect(formatBytes(852)).toBe('852 B');
  });

  it('handles zero and invalid input', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(-5)).toBe('0 B');
    expect(formatBytes(Number.NaN)).toBe('0 B');
  });
});
