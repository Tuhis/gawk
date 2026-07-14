import { describe, expect, it } from 'vitest';
import { fmt, fmtBits, fmtInt, fmtOr } from './format';

describe('format helpers', () => {
  it('fmt renders finite numbers and — otherwise', () => {
    expect(fmt(1.234)).toBe('1.2');
    expect(fmt(NaN)).toBe('—');
  });

  it('nullable variants render — for null/undefined (R9 connection stats)', () => {
    expect(fmtOr(null)).toBe('—');
    expect(fmtOr(3.21)).toBe('3.2');
    expect(fmtInt(undefined)).toBe('—');
    expect(fmtInt(7.6)).toBe('8');
  });

  it('fmtBits scales bits per second', () => {
    expect(fmtBits(null)).toBe('—');
    expect(fmtBits(500)).toBe('500 bps');
    expect(fmtBits(48_000)).toBe('48 kbps');
    expect(fmtBits(4_200_000)).toBe('4.2 Mbps');
  });
});
