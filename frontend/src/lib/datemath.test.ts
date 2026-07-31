import { describe, expect, it } from 'vitest';
import dayjs from 'dayjs';
import { isValid, parse } from './datemath';

// A fixed anchor keeps every assertion deterministic. Expectations that involve
// rounding are derived with dayjs rather than hard-coded, so the suite does not
// depend on the machine's timezone.
const NOW = new Date('2026-07-15T13:37:42.500Z');
const at = (text: string, roundUp = false) => parse(text, { forceNow: NOW, roundUp });

describe('anchors', () => {
  it('resolves `now` to forceNow', () => {
    expect(at('now')?.valueOf()).toBe(NOW.valueOf());
  });

  it('parses a bare ISO8601 timestamp', () => {
    expect(at('2026-01-15T00:00:00Z')?.toISOString()).toBe('2026-01-15T00:00:00.000Z');
  });

  it('parses an ISO anchor with `||` math', () => {
    expect(at('2026-01-15T00:00:00Z||+1d')?.toISOString()).toBe('2026-01-16T00:00:00.000Z');
  });

  it('rejects an unparseable anchor', () => {
    expect(at('not-a-date||+1d')).toBeUndefined();
  });

  it('rejects empty input', () => {
    expect(at('')).toBeUndefined();
  });
});

describe('immutability (regression: moment mutated in place, dayjs does not)', () => {
  // The upstream moment implementation discarded the return value of
  // add/subtract/startOf. Ported naively onto immutable dayjs that silently
  // yields `now` for every expression. These guard against a reintroduction.
  it('actually moves the clock for subtraction', () => {
    expect(at('now-1h')?.valueOf()).toBe(NOW.valueOf() - 3_600_000);
    expect(at('now-1h')?.valueOf()).not.toBe(NOW.valueOf());
  });

  it('actually moves the clock for addition', () => {
    expect(at('now+30m')?.valueOf()).toBe(NOW.valueOf() + 1_800_000);
  });

  it('actually rounds', () => {
    expect(at('now/d')?.valueOf()).not.toBe(NOW.valueOf());
    expect(at('now/d')?.valueOf()).toBe(dayjs(NOW).startOf('day').valueOf());
  });

  it('applies every operation in a chain', () => {
    // If any single step were dropped this would collapse to an earlier value.
    const want = dayjs(NOW).subtract(1, 'day').startOf('day').add(6, 'hour');
    expect(at('now-1d/d+6h')?.valueOf()).toBe(want.valueOf());
  });
});

describe('offsets', () => {
  it('subtracts hours', () => {
    expect(at('now-24h')?.valueOf()).toBe(NOW.valueOf() - 86_400_000);
  });

  it('subtracts minutes', () => {
    expect(at('now-5m')?.valueOf()).toBe(NOW.valueOf() - 300_000);
  });

  it('handles multi-digit counts', () => {
    expect(at('now-90d')?.valueOf()).toBe(dayjs(NOW).subtract(90, 'day').valueOf());
  });

  it('defaults an omitted count to 1', () => {
    expect(at('now-d')?.valueOf()).toBe(at('now-1d')?.valueOf());
    expect(at('now+h')?.valueOf()).toBe(at('now+1h')?.valueOf());
  });

  it('supports every unit', () => {
    const cases: [string, dayjs.ManipulateType][] = [
      ['ms', 'ms'],
      ['s', 's'],
      ['m', 'm'],
      ['h', 'h'],
      ['d', 'd'],
      ['w', 'w'],
      ['M', 'M'],
      ['y', 'y'],
    ];
    for (const [suffix, unit] of cases) {
      expect(at(`now-3${suffix}`)?.valueOf(), suffix).toBe(
        dayjs(NOW).subtract(3, unit).valueOf(),
      );
    }
  });

  it('treats `m` as minutes and `M` as months', () => {
    expect(at('now-1m')?.valueOf()).toBe(dayjs(NOW).subtract(1, 'minute').valueOf());
    expect(at('now-1M')?.valueOf()).toBe(dayjs(NOW).subtract(1, 'month').valueOf());
    expect(at('now-1m')?.valueOf()).not.toBe(at('now-1M')?.valueOf());
  });
});

describe('rounding', () => {
  it('snaps down by default', () => {
    expect(at('now/M')?.valueOf()).toBe(dayjs(NOW).startOf('month').valueOf());
  });

  it('snaps up when roundUp is set', () => {
    expect(at('now/d', true)?.valueOf()).toBe(dayjs(NOW).endOf('day').valueOf());
  });

  it('gives start of yesterday for now-1d/d', () => {
    expect(at('now-1d/d')?.valueOf()).toBe(dayjs(NOW).subtract(1, 'day').startOf('day').valueOf());
  });

  it('accepts an explicit count of 1', () => {
    expect(at('now/1d')?.valueOf()).toBe(at('now/d')?.valueOf());
  });

  it('rejects rounding by more than one unit', () => {
    expect(at('now/2d')).toBeUndefined();
  });
});

describe('malformed input', () => {
  it.each([
    ['now-5x', 'unknown unit'],
    ['now-5', 'missing unit'],
    ['now-', 'trailing operator'],
    ['now*1d', 'bad operator'],
    ['now-1d/', 'trailing round'],
  ])('rejects %s (%s)', (text) => {
    expect(at(text)).toBeUndefined();
  });
});

describe('isValid', () => {
  it('agrees with parse', () => {
    expect(isValid('now-7d', { forceNow: NOW })).toBe(true);
    expect(isValid('now-7q', { forceNow: NOW })).toBe(false);
  });
});

describe('forceNow validation', () => {
  it('throws on an invalid Date', () => {
    expect(() => parse('now', { forceNow: new Date('nope') })).toThrow(/valid Date/);
  });

  it('uses the real clock when omitted', () => {
    const before = Date.now();
    const got = parse('now')?.valueOf() ?? 0;
    expect(got).toBeGreaterThanOrEqual(before);
    expect(got).toBeLessThanOrEqual(Date.now());
  });
});
