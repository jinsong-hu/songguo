import { describe, expect, it } from 'vitest';
import dayjs from 'dayjs';
import {
  DEFAULT_RANGE,
  PRESETS,
  TARGET_POINTS,
  type TimeRange,
  deriveBucket,
  calendarBounds,
  calendarSelection,
  formatInput,
  isRolling,
  rangeFromCalendar,
  rangeFromInputs,
  rangeLabel,
  rangeToInputs,
  resolveRange,
} from './timeRange';

const NOW = new Date('2026-07-15T13:37:42.500Z');
const sec = (d: dayjs.Dayjs) => Math.floor(d.valueOf() / 1000);

describe('resolveRange — rolling', () => {
  it('resolves a relative window against the pinned clock', () => {
    const got = resolveRange({ kind: 'rolling', from: 'now-24h', to: 'now' }, NOW);
    expect(got).not.toBeNull();
    expect(got!.until - got!.since).toBe(86_400);
    expect(got!.until).toBe(Math.floor(NOW.valueOf() / 1000));
  });

  it('rounds the upper bound up so a snapped range covers its last unit', () => {
    // `now/d` on both ends is Grafana's "Today": start of day → end of day, not
    // an empty midnight-to-midnight window.
    const got = resolveRange({ kind: 'rolling', from: 'now/d', to: 'now/d' }, NOW);
    expect(got).not.toBeNull();
    expect(got!.since).toBe(sec(dayjs(NOW).startOf('day')));
    expect(got!.until).toBe(sec(dayjs(NOW).endOf('day')));
    expect(got!.until - got!.since).toBeGreaterThan(86_000);
  });

  it('resolves Yesterday to the previous whole day', () => {
    const got = resolveRange({ kind: 'rolling', from: 'now-1d/d', to: 'now-1d/d' }, NOW);
    expect(got!.since).toBe(sec(dayjs(NOW).subtract(1, 'day').startOf('day')));
    expect(got!.until).toBe(sec(dayjs(NOW).subtract(1, 'day').endOf('day')));
  });

  it('returns null for an unparseable expression', () => {
    expect(resolveRange({ kind: 'rolling', from: 'now-5q', to: 'now' }, NOW)).toBeNull();
    expect(resolveRange({ kind: 'rolling', from: 'garbage', to: 'now' }, NOW)).toBeNull();
  });

  it('returns null for an inverted window', () => {
    expect(resolveRange({ kind: 'rolling', from: 'now', to: 'now-1h' }, NOW)).toBeNull();
  });
});

describe('resolveRange — absolute', () => {
  it('passes pinned seconds through untouched', () => {
    expect(resolveRange({ kind: 'absolute', from: 1_000, to: 2_000 })).toEqual({
      since: 1_000,
      until: 2_000,
    });
  });

  it('does not move with the clock', () => {
    const range: TimeRange = { kind: 'absolute', from: 1_000, to: 2_000 };
    const a = resolveRange(range, new Date('2026-01-01T00:00:00Z'));
    const b = resolveRange(range, new Date('2027-01-01T00:00:00Z'));
    expect(a).toEqual(b);
  });

  it('rejects empty and inverted windows', () => {
    expect(resolveRange({ kind: 'absolute', from: 2_000, to: 2_000 })).toBeNull();
    expect(resolveRange({ kind: 'absolute', from: 3_000, to: 2_000 })).toBeNull();
  });
});

describe('deriveBucket', () => {
  it.each([
    [3_600, '1m'],
    [6 * 3_600, '5m'],
    [86_400, '15m'],
    [7 * 86_400, 'hour'],
    [30 * 86_400, '3h'],
    [90 * 86_400, '12h'],
  ])('picks a sensible size for a %ss span', (span, want) => {
    expect(deriveBucket(0, span)).toBe(want);
  });

  it('never exceeds the point target', () => {
    const sizes: Record<string, number> = {
      '1m': 60,
      '5m': 300,
      '15m': 900,
      '30m': 1800,
      hour: 3600,
      '3h': 10800,
      '6h': 21600,
      '12h': 43200,
      day: 86400,
      '7d': 604800,
      '30d': 2592000,
    };
    for (let span = 60; span < 400 * 86_400; span = Math.ceil(span * 1.7)) {
      const bucket = deriveBucket(0, span);
      expect(sizes[bucket], `unknown bucket ${bucket}`).toBeDefined();
      expect(span / sizes[bucket], `span ${span}`).toBeLessThanOrEqual(TARGET_POINTS);
    }
  });

  it('never asks for a size the API would reject', () => {
    // Mirrors the Go side: hour/day, or <count><unit> in whole minutes, <= 365d.
    for (let span = 1; span < 400 * 86_400; span = Math.ceil(span * 1.7)) {
      const bucket = deriveBucket(0, span);
      expect(bucket).toMatch(/^(hour|day|\d+[mhd])$/);
    }
  });

  it('floors at 1m for tiny and degenerate spans', () => {
    expect(deriveBucket(0, 1)).toBe('1m');
    expect(deriveBucket(0, 0)).toBe('1m');
    expect(deriveBucket(500, 100)).toBe('1m');
  });
});

describe('presets', () => {
  it('all resolve to a usable window', () => {
    for (const p of PRESETS) {
      const got = resolveRange(p.range, NOW);
      expect(got, p.label).not.toBeNull();
      expect(got!.until, p.label).toBeGreaterThan(got!.since);
    }
  });

  it('all stay within the 90d retention horizon', () => {
    const oldest = Math.floor(NOW.valueOf() / 1000) - 90 * 86_400;
    for (const p of PRESETS) {
      // A day of slack: "Last 90 days" and snapped month ranges can reach a few
      // hours past a naive 90d cut without being meaningfully older.
      expect(resolveRange(p.range, NOW)!.since, p.label).toBeGreaterThan(oldest - 86_400 * 32);
    }
  });

  it('all produce a chart under the point target', () => {
    for (const p of PRESETS) {
      const { since, until } = resolveRange(p.range, NOW)!;
      expect(deriveBucket(since, until), p.label).toMatch(/^(hour|day|\d+[mhd])$/);
    }
  });
});

describe('rangeLabel', () => {
  it('prefers the preset name', () => {
    expect(rangeLabel({ kind: 'rolling', from: 'now-24h', to: 'now' })).toBe('Last 24 hours');
    expect(rangeLabel({ kind: 'rolling', from: 'now-1d/d', to: 'now-1d/d' })).toBe('Yesterday');
    expect(rangeLabel({ kind: 'rolling', from: 'now/M', to: 'now/M' })).toBe('This month');
  });

  it('spells out a custom relative window', () => {
    expect(rangeLabel({ kind: 'rolling', from: 'now-45m', to: 'now' })).toBe('Last 45 minutes');
    expect(rangeLabel({ kind: 'rolling', from: 'now-1w', to: 'now' })).toBe('Last 1 week');
    expect(rangeLabel({ kind: 'rolling', from: 'now-2y', to: 'now' })).toBe('Last 2 years');
  });

  it('falls back to the raw expressions when it cannot do better', () => {
    expect(rangeLabel({ kind: 'rolling', from: 'now-1d/d', to: 'now-3h' })).toBe(
      'now-1d/d → now-3h',
    );
  });

  it('formats an absolute range as two timestamps', () => {
    const from = dayjs('2026-07-14T09:00:00').valueOf() / 1000;
    const to = dayjs('2026-07-21T09:00:00').valueOf() / 1000;
    expect(rangeLabel({ kind: 'absolute', from, to })).toBe('2026-07-14 09:00 → 2026-07-21 09:00');
  });
});

describe('rangeFromInputs', () => {
  it('keeps a range rolling when either side mentions now', () => {
    expect(rangeFromInputs('now-6h', 'now')).toEqual({
      kind: 'rolling',
      from: 'now-6h',
      to: 'now',
    });
    expect(rangeFromInputs('2026-07-14T09:00:00Z', 'now')).toEqual({
      kind: 'rolling',
      from: '2026-07-14T09:00:00Z',
      to: 'now',
    });
  });

  it('pins two timestamps to absolute seconds', () => {
    const got = rangeFromInputs('2026-07-14T09:00:00Z', '2026-07-21T09:00:00Z');
    expect(got).toEqual({
      kind: 'absolute',
      from: Math.floor(Date.parse('2026-07-14T09:00:00Z') / 1000),
      to: Math.floor(Date.parse('2026-07-21T09:00:00Z') / 1000),
    });
  });

  it('trims surrounding whitespace', () => {
    expect(rangeFromInputs('  now-6h  ', ' now ')).toEqual({
      kind: 'rolling',
      from: 'now-6h',
      to: 'now',
    });
  });

  it.each([
    ['', 'now', 'empty from'],
    ['now-6h', '', 'empty to'],
    ['   ', 'now', 'blank from'],
    ['now-5q', 'now', 'bad unit'],
    ['garbage', 'now', 'unparseable'],
    ['now', 'now-6h', 'inverted'],
  ])('rejects (%s, %s) — %s', (from, to) => {
    expect(rangeFromInputs(from, to)).toBeNull();
  });
});

describe('rangeToInputs', () => {
  it('round-trips a rolling range verbatim', () => {
    const range: TimeRange = { kind: 'rolling', from: 'now-1d/d', to: 'now/d' };
    expect(rangeToInputs(range)).toEqual({ from: 'now-1d/d', to: 'now/d' });
    expect(rangeFromInputs('now-1d/d', 'now/d')).toEqual(range);
  });

  it('round-trips an absolute range through its formatted form', () => {
    const from = Math.floor(dayjs('2026-07-14T09:00:00').valueOf() / 1000);
    const to = Math.floor(dayjs('2026-07-21T09:00:00').valueOf() / 1000);
    const fields = rangeToInputs({ kind: 'absolute', from, to });
    expect(fields).toEqual({ from: '2026-07-14 09:00', to: '2026-07-21 09:00' });
    expect(rangeFromInputs(fields.from, fields.to)).toEqual({ kind: 'absolute', from, to });
  });
});

describe('calendar helpers', () => {
  it('highlights the window the fields currently mean, relative included', () => {
    const got = calendarSelection('now-7d', 'now', NOW);
    expect(got).toBeDefined();
    expect(Math.round((got!.to.valueOf() - got!.from.valueOf()) / 1000)).toBe(7 * 86_400);
  });

  it('highlights an absolute window', () => {
    const got = calendarSelection('2026-07-14 09:00', '2026-07-16 09:00', NOW);
    expect(formatInput(got!.from)).toBe('2026-07-14 09:00');
    expect(formatInput(got!.to)).toBe('2026-07-16 09:00');
  });

  it('has no selection while the fields are unusable', () => {
    expect(calendarSelection('', 'now', NOW)).toBeUndefined();
    expect(calendarSelection('garbage', 'now', NOW)).toBeUndefined();
  });

  it('widens a picked day range to its edges', () => {
    const picked = rangeFromCalendar(
      new Date('2026-07-14T11:22:33'),
      new Date('2026-07-16T04:05:06'),
    );
    // Without the widening the final day would be truncated to midnight and the
    // chart would silently omit it.
    expect(picked.from).toBe('2026-07-14 00:00');
    expect(picked.to).toBe('2026-07-16 23:59');
  });

  it('treats a single clicked day as that whole day', () => {
    const picked = rangeFromCalendar(new Date('2026-07-14T11:22:33'));
    expect(picked).toEqual({ from: '2026-07-14 00:00', to: '2026-07-14 23:59' });
  });

  it('produces fields that pin to an absolute range', () => {
    const picked = rangeFromCalendar(new Date('2026-07-14T00:00:00'));
    expect(rangeFromInputs(picked.from, picked.to)?.kind).toBe('absolute');
  });

  it('bounds the grid to the 90-day retention horizon and today', () => {
    const { fromDate, toDate } = calendarBounds(NOW);
    expect(fromDate.valueOf()).toBe(dayjs(NOW).subtract(90, 'day').startOf('day').valueOf());
    expect(toDate.valueOf()).toBe(dayjs(NOW).endOf('day').valueOf());
  });
});

describe('defaults', () => {
  it('DEFAULT_RANGE matches the old three-tab starting point', () => {
    expect(rangeLabel(DEFAULT_RANGE)).toBe('Last 24 hours');
    expect(resolveRange(DEFAULT_RANGE, NOW)!.until - resolveRange(DEFAULT_RANGE, NOW)!.since).toBe(
      86_400,
    );
  });

  it('isRolling distinguishes the two kinds', () => {
    expect(isRolling(DEFAULT_RANGE)).toBe(true);
    expect(isRolling({ kind: 'absolute', from: 1, to: 2 })).toBe(false);
  });
});
