// Formatting helpers shared across pages.

const moneyFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

const moneyFineFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
});

/** Format a dollar amount, e.g. $1,234.56. Small values keep extra precision. */
export function money(n: number): string {
  if (n !== 0 && Math.abs(n) < 0.01) return moneyFineFmt.format(n);
  return moneyFmt.format(n);
}

/** Format latency in milliseconds, e.g. "123 ms". */
export function ms(n: number): string {
  return `${Math.round(n).toLocaleString('en-US')} ms`;
}

const intFmt = new Intl.NumberFormat('en-US');

export function int(n: number): string {
  return intFmt.format(Math.round(n));
}

/** Format a byte count into a compact human string, e.g. "32 KB", "1.5 MB". */
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let val = n / 1024;
  let i = 0;
  while (val >= 1024 && i < units.length - 1) {
    val /= 1024;
    i += 1;
  }
  const rounded = val >= 100 || Number.isInteger(val) ? Math.round(val) : Math.round(val * 10) / 10;
  return `${rounded} ${units[i]}`;
}

/** Format an error rate fraction (0..1) as a percent, e.g. "2.4%". */
export function percent(fraction: number): string {
  return `${(fraction * 100).toFixed(1)}%`;
}

/**
 * Format a duration given in seconds as a compact human string, e.g. "45s",
 * "3m 20s", "1h 4m". Zero and sub-second values render as "0s".
 */
export function duration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0s';
  const s = Math.round(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

const dateTimeFmt = new Intl.DateTimeFormat(undefined, {
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

/** Compact local datetime from an RFC3339 string. */
export function dateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return dateTimeFmt.format(d);
}

const timeFmt = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

const dayFmt = new Intl.DateTimeFormat(undefined, {
  month: 'short',
  day: 'numeric',
});

/**
 * Axis/tooltip label for a series point, scaled to the bucket size.
 *
 * Anything a day or coarser is labelled by date; everything finer — hourly and
 * the minute sizes — by time of day. Written as "is it day-scale?" rather than
 * "is it hourly?" so new sub-hour sizes fall on the correct side by default.
 */
export function bucketLabel(iso: string, bucket: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const dayScale = bucket === 'day' || /^\d+d$/.test(bucket);
  return dayScale ? dayFmt.format(d) : timeFmt.format(d);
}

/**
 * Duration of a call, or null when the call has no duration to report yet.
 *
 * A call still in flight has `latency_ms = 0` from the create-at-start row, and
 * rendering that as "0s" claims it finished instantly. Callers that have the
 * start timestamp should use `elapsedSince` instead; the rest render "—".
 */
export function callDuration(latencyMs: number, pending: boolean): string | null {
  if (pending) return null;
  return duration(latencyMs / 1000);
}

/** Wall-clock time since an RFC3339 instant, for a call that is still running. */
export function elapsedSince(iso: string, now: number = Date.now()): string {
  const started = new Date(iso).getTime();
  if (Number.isNaN(started)) return '—';
  return duration(Math.max(0, now - started) / 1000);
}
