// The dashboard's time range model.
//
// The API only ever speaks absolute unix seconds (`since`/`until`), so the
// rolling-vs-absolute distinction lives entirely here. That split is the whole
// point of the type below:
//
//   - a ROLLING range is a pair of datemath expressions re-resolved against the
//     clock on every refresh, because "Last 24 hours" has to keep meaning the
//     last 24 hours;
//   - an ABSOLUTE range is two pinned instants that must NOT drift when the page
//     refreshes, because "the 14th" is not a moving target.
//
// Callers must therefore keep the live tick in their resolve dependencies for
// rolling ranges and out of them for absolute ones.

import dayjs from 'dayjs';
import { parse } from './datemath';

export type TimeRange =
  | { kind: 'rolling'; from: string; to: string }
  | { kind: 'absolute'; from: number; to: number };

/** A range collapsed to what the API takes: inclusive-exclusive unix seconds. */
export interface ResolvedRange {
  since: number;
  until: number;
}

/**
 * Resolve a range to unix seconds, or null when it is unusable — an
 * unparseable expression, or a window that is empty or inverted. Null means
 * "do not fetch"; it is a normal state while someone is mid-typing, not an error.
 *
 * `now` pins the clock for rolling ranges; omit it outside tests.
 */
export function resolveRange(range: TimeRange, now?: Date): ResolvedRange | null {
  if (range.kind === 'absolute') {
    return range.from < range.to ? { since: range.from, until: range.to } : null;
  }

  // The upper bound rounds up so a snapped range covers its whole final unit:
  // `now/d` → `now/d` is all of today rather than an empty midnight-to-midnight.
  const from = parse(range.from, { forceNow: now, roundUp: false });
  const to = parse(range.to, { forceNow: now, roundUp: true });
  if (!from || !to) return null;

  const since = Math.floor(from.valueOf() / 1000);
  const until = Math.floor(to.valueOf() / 1000);
  return since < until ? { since, until } : null;
}

/**
 * The size ladder, finest first: a span at or under `upTo` seconds gets `size`,
 * and anything past the last rung gets `day`. Four fixed rungs rather than the
 * finest size fitting a point budget — a window always draws the same bars, so
 * comparing two views is comparing like with like instead of the granularity
 * shifting under you as the window is nudged. `hour` and `day` keep their word
 * spellings because that is what the API has always called them.
 */
const BUCKETS: { upTo: number; size: string }[] = [
  { upTo: 30 * 60, size: '1m' },
  { upTo: 10 * 3600, size: '30m' },
  // The one exclusive edge: a span of exactly seven days is daily, not hourly.
  { upTo: 7 * 86400 - 1, size: 'hour' },
];

/**
 * The bucket a range gets: **1m up to half an hour, 30m up to ten hours, hourly
 * under a week, daily at a week or beyond**.
 *
 * The rungs are chosen so each window reads at a useful density — 30 bars for
 * half an hour, 20 for ten hours, 24 for a day, 168 for a week, 90 for the
 * 90-day retention horizon. The cost is that the widest windows draw more
 * points than a coarser roll-up would; well within what a chart can carry, and
 * far under the store's 10000-bucket cap.
 */
export function deriveBucket(since: number, until: number): string {
  const span = Math.max(0, until - since);
  for (const b of BUCKETS) {
    if (span <= b.upTo) return b.size;
  }
  return 'day';
}

export interface Preset {
  label: string;
  range: Extract<TimeRange, { kind: 'rolling' }>;
}

/**
 * The preset rail. Snapped entries ("Today", "Yesterday") use the same
 * expression on both ends and lean on resolveRange's roundUp to cover the unit.
 *
 * Deliberately a coarse ~4× ladder rather than every window someone might want:
 * the two free-text fields already cover the in-between rungs, so listing
 * 30 minutes beside 15 and 60 buys nothing and costs a rail long enough to have
 * to scan. Only the ends are load-bearing — 5 minutes because that is "is it
 * working right now", 90 days because the janitor prunes calls at 90d and so
 * that is the widest window with any data in it.
 */
export const PRESETS: Preset[] = [
  { label: 'Last 5 minutes', range: { kind: 'rolling', from: 'now-5m', to: 'now' } },
  { label: 'Last 15 minutes', range: { kind: 'rolling', from: 'now-15m', to: 'now' } },
  { label: 'Last 1 hour', range: { kind: 'rolling', from: 'now-1h', to: 'now' } },
  { label: 'Last 6 hours', range: { kind: 'rolling', from: 'now-6h', to: 'now' } },
  { label: 'Last 24 hours', range: { kind: 'rolling', from: 'now-24h', to: 'now' } },
  { label: 'Last 7 days', range: { kind: 'rolling', from: 'now-7d', to: 'now' } },
  { label: 'Last 30 days', range: { kind: 'rolling', from: 'now-30d', to: 'now' } },
  { label: 'Last 90 days', range: { kind: 'rolling', from: 'now-90d', to: 'now' } },
  { label: 'Today', range: { kind: 'rolling', from: 'now/d', to: 'now/d' } },
  { label: 'Yesterday', range: { kind: 'rolling', from: 'now-1d/d', to: 'now-1d/d' } },
  { label: 'This month', range: { kind: 'rolling', from: 'now/M', to: 'now/M' } },
];

/** The default on first load — what the old three-tab control started on. */
export const DEFAULT_RANGE: TimeRange = { kind: 'rolling', from: 'now-24h', to: 'now' };

const UNIT_NAMES: Record<string, string> = {
  s: 'second',
  m: 'minute',
  h: 'hour',
  d: 'day',
  w: 'week',
  M: 'month',
  y: 'year',
};

const ABSOLUTE_FMT = 'YYYY-MM-DD HH:mm';

/**
 * A human label for the picker's trigger button. Presets win so snapped ranges
 * read as "Yesterday" rather than as their expressions; a plain `now-N<unit>`
 * gets spelled out; anything else falls back to showing the raw expressions,
 * which is the honest thing to do for a range someone hand-wrote.
 */
export function rangeLabel(range: TimeRange): string {
  if (range.kind === 'absolute') {
    const from = dayjs(range.from * 1000).format(ABSOLUTE_FMT);
    const to = dayjs(range.to * 1000).format(ABSOLUTE_FMT);
    return `${from} → ${to}`;
  }

  const preset = PRESETS.find((p) => p.range.from === range.from && p.range.to === range.to);
  if (preset) return preset.label;

  if (range.to === 'now') {
    const m = /^now-(\d+)([smhdwMy])$/.exec(range.from);
    if (m) {
      const n = Number(m[1]);
      const unit = UNIT_NAMES[m[2]];
      if (unit) return `Last ${n} ${unit}${n === 1 ? '' : 's'}`;
    }
  }

  return `${range.from} → ${range.to}`;
}

/** True when the range must be re-resolved as the clock moves. */
export function isRolling(range: TimeRange): boolean {
  return range.kind === 'rolling';
}

/**
 * Build a range from the picker's two free-text fields, or null if either side
 * is unusable. Each field takes a datemath expression or a timestamp, which is
 * what makes the control feel like Grafana's.
 *
 * The kind is inferred rather than toggled: a field mentioning `now` is only
 * meaningful if it keeps being re-evaluated, so any mention makes the whole
 * range rolling. Two plain timestamps pin to absolute. That means "now-6h" to
 * "now" keeps sliding while "2026-07-14 09:00" to "now" also slides (its upper
 * edge genuinely is the clock) — and neither surprises the person who typed it.
 */
export function rangeFromInputs(from: string, to: string): TimeRange | null {
  const f = from.trim();
  const t = to.trim();
  if (!f || !t) return null;

  const rolling: TimeRange = { kind: 'rolling', from: f, to: t };
  const resolved = resolveRange(rolling);
  if (!resolved) return null;

  if (f.includes('now') || t.includes('now')) return rolling;
  return { kind: 'absolute', from: resolved.since, to: resolved.until };
}

/** The inverse: seed the picker's fields from the active range. */
export function rangeToInputs(range: TimeRange): { from: string; to: string } {
  if (range.kind === 'rolling') return { from: range.from, to: range.to };
  return {
    from: dayjs(range.from * 1000).format(ABSOLUTE_FMT),
    to: dayjs(range.to * 1000).format(ABSOLUTE_FMT),
  };
}

/** Render a Date in the form the picker's fields use. */
export function formatInput(d: Date): string {
  return dayjs(d).format(ABSOLUTE_FMT);
}

/**
 * The window the calendar should highlight for the current field contents.
 *
 * Deliberately resolves the fields rather than reading only absolute ranges, so
 * the grid also shades what a relative expression currently covers — typing
 * `now-7d` lights up the last seven days. Undefined when the fields do not yet
 * make a range.
 */
export function calendarSelection(
  from: string,
  to: string,
  now?: Date,
): { from: Date; to: Date } | undefined {
  const candidate = rangeFromInputs(from, to);
  if (!candidate) return undefined;
  const resolved = resolveRange(candidate, now);
  if (!resolved) return undefined;
  return { from: new Date(resolved.since * 1000), to: new Date(resolved.until * 1000) };
}

/**
 * Field contents for a range picked on the calendar. The grid has day
 * granularity, so the days are widened to their edges — anything else would
 * silently truncate the last day to midnight.
 *
 * The result contains no `now`, so applying it pins the range to absolute,
 * which is what clicking specific dates on a calendar means.
 */
export function rangeFromCalendar(from: Date, to?: Date): { from: string; to: string } {
  const start = dayjs(from).startOf('day');
  const end = dayjs(to ?? from).endOf('day');
  return { from: start.format(ABSOLUTE_FMT), to: end.format(ABSOLUTE_FMT) };
}

/**
 * Days the calendar should refuse. Nothing in the future has data, and the
 * janitor prunes calls at 90 days, so an older pick would render an honest but
 * empty chart. Matches the reach of the preset rail.
 */
export function calendarBounds(now: Date = new Date()): { fromDate: Date; toDate: Date } {
  return {
    fromDate: dayjs(now).subtract(90, 'day').startOf('day').toDate(),
    toDate: dayjs(now).endOf('day').toDate(),
  };
}
