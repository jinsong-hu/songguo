// Elasticsearch/Grafana-style date math, e.g. `now`, `now-24h`, `now-1d/d`.
//
// Ported from elastic/datemath-js (Apache-2.0, https://github.com/elastic/datemath-js),
// which is unmaintained and depends on moment. This version drops moment for
// dayjs and fixes two things the original got away with:
//
//   1. moment is mutable, so the original discarded the return value of
//      add/subtract/startOf. dayjs is immutable — every call MUST be reassigned
//      or the math silently evaporates and every expression resolves to `now`.
//   2. a dead `mathString.length === 2` branch left `num` as a string.
//
// Grammar: an anchor (`now`, or an ISO8601 timestamp followed by `||`) plus zero
// or more operations — `+<n><unit>`, `-<n><unit>`, or `/<unit>` to round. The
// count is optional and defaults to 1, so `now-d` == `now-1d`. Rounding is only
// legal on a single whole unit: `/d` is fine, `/2d` is not.

import dayjs, { type Dayjs, type ManipulateType, type OpUnitType } from 'dayjs';

/**
 * Units the grammar accepts, in the case-sensitive Elasticsearch spelling —
 * note `m` is minutes and `M` is months.
 *
 * Caveat on `w`: dayjs starts the week per the active locale (Sunday by
 * default), so `now/w` is not ISO weeks. Add dayjs's `isoWeek` plugin if that
 * ever matters; nothing in the app rounds by week today.
 */
const UNITS = ['ms', 's', 'm', 'h', 'd', 'w', 'M', 'y'] as const;

export type DateMathUnit = (typeof UNITS)[number];

const UNIT_SET = new Set<string>(UNITS);

export interface ParseOptions {
  /**
   * How `/` rounds. `false` (default) snaps to the start of the unit, `true` to
   * the end — use `true` for the upper bound of a range so `now/d` covers all of
   * today rather than collapsing to midnight.
   */
  roundUp?: boolean;
  /** Pins what `now` means. Omit for the real clock; pass a Date to make parsing deterministic. */
  forceNow?: Date;
}

const isDate = (v: unknown): v is Date => Object.prototype.toString.call(v) === '[object Date]';

const isDigit = (c: string): boolean => c >= '0' && c <= '9';

const isAlpha = (c: string): boolean => (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z');

/**
 * Parse a date math expression. Returns `undefined` for anything malformed —
 * an unknown unit, a rounding with a count, a trailing operator — so callers can
 * treat `undefined` as "invalid input" and show it inline.
 *
 * Note `m` is minutes and `M` is months, exactly as in Elasticsearch.
 */
export function parse(text: string, opts: ParseOptions = {}): Dayjs | undefined {
  const { roundUp = false, forceNow } = opts;

  if (!text) return undefined;
  if (forceNow !== undefined && !(isDate(forceNow) && !isNaN(forceNow.valueOf()))) {
    throw new Error('datemath: forceNow must be a valid Date');
  }

  let time: Dayjs;
  let mathString: string;

  if (text.startsWith('now')) {
    time = dayjs(forceNow);
    mathString = text.slice(3);
  } else {
    // An explicit anchor is `<iso8601>||<math>`; without the `||` the whole
    // string is the timestamp. Only ISO8601 is supported, as upstream.
    const sep = text.indexOf('||');
    const parseString = sep === -1 ? text : text.slice(0, sep);
    mathString = sep === -1 ? '' : text.slice(sep + 2);
    time = dayjs(parseString);
    if (!time.isValid()) return undefined;
  }

  if (!mathString.length) return time;
  return applyMath(mathString, time, roundUp);
}

/** True when `text` is a well-formed expression. Thin wrapper over {@link parse}. */
export function isValid(text: string, opts: ParseOptions = {}): boolean {
  return parse(text, opts) !== undefined;
}

function applyMath(mathString: string, anchor: Dayjs, roundUp: boolean): Dayjs | undefined {
  let dateTime = anchor;
  const len = mathString.length;
  let i = 0;

  while (i < len) {
    const op = mathString.charAt(i++);
    if (op !== '/' && op !== '+' && op !== '-') return undefined;

    // Optional count; absent means 1 so `now-d` == `now-1d`.
    const numStart = i;
    while (i < len && isDigit(mathString.charAt(i))) i++;
    const num = i > numStart ? parseInt(mathString.slice(numStart, i), 10) : 1;

    // Rounding is only defined for a single whole unit: `/d` and `/1d` are fine,
    // `/2d` is meaningless.
    if (op === '/' && num !== 1) return undefined;

    const unitStart = i;
    while (i < len && isAlpha(mathString.charAt(i))) i++;
    const unit = mathString.slice(unitStart, i);
    if (!UNIT_SET.has(unit)) return undefined;

    // Reassign, always: dayjs is immutable. See the note at the top of the file.
    if (op === '/') {
      dateTime = roundUp
        ? dateTime.endOf(unit as OpUnitType)
        : dateTime.startOf(unit as OpUnitType);
    } else if (op === '+') {
      dateTime = dateTime.add(num, unit as ManipulateType);
    } else {
      dateTime = dateTime.subtract(num, unit as ManipulateType);
    }
  }

  return dateTime;
}
