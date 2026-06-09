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

/** Axis/tooltip label for a series point, scaled to the bucket size. */
export function bucketLabel(iso: string, bucket: 'hour' | 'day'): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return bucket === 'hour' ? timeFmt.format(d) : dayFmt.format(d);
}

/** Status group for a call entry, mapping to a pill style. */
export function statusKind(status: number): 'ok' | 'warn' | 'err' {
  if (status >= 200 && status < 300) return 'ok';
  if (status >= 400 && status < 500) return 'warn';
  return 'err';
}
