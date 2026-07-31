import { useEffect, useRef, useState } from 'react';
import { Clock } from 'lucide-react';
import {
  PRESETS,
  type TimeRange,
  rangeFromInputs,
  rangeLabel,
  rangeToInputs,
  resolveRange,
} from '../lib/timeRange';
import styles from './TimeRangePicker.module.css';

/**
 * Grafana-shaped time range control: a preset rail beside two free-text fields
 * that take either a datemath expression (`now-6h`, `now-1d/d`) or a timestamp.
 *
 * The two fields are the whole reason this is not a plain dropdown — they are
 * what lets someone pin an exact window without the control having to enumerate
 * every window anyone might want.
 */
export function TimeRangePicker({
  value,
  onChange,
}: {
  value: TimeRange;
  onChange: (next: TimeRange) => void;
}) {
  const [open, setOpen] = useState(false);
  const [from, setFrom] = useState(() => rangeToInputs(value).from);
  const [to, setTo] = useState(() => rangeToInputs(value).to);
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const firstFieldRef = useRef<HTMLInputElement>(null);

  // Reseed the fields whenever the panel opens, so they always show the range
  // actually in effect rather than an abandoned edit from last time.
  useEffect(() => {
    if (!open) return;
    const seeded = rangeToInputs(value);
    setFrom(seeded.from);
    setTo(seeded.to);
    setError(null);
    firstFieldRef.current?.focus();
  }, [open, value]);

  // Dismiss on outside pointerdown or Escape. Escape is listened for on the
  // document rather than the panel so it works wherever focus has wandered.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open]);

  const apply = () => {
    const next = rangeFromInputs(from, to);
    if (!next) {
      setError('Enter a time range — e.g. now-6h, now/d, or 2026-07-14 09:00.');
      return;
    }
    onChange(next);
    setOpen(false);
  };

  const choosePreset = (range: TimeRange) => {
    onChange(range);
    setOpen(false);
  };

  // A live preview of what the typed range resolves to, so an expression can be
  // checked before it is applied.
  const preview = (() => {
    const candidate = rangeFromInputs(from, to);
    if (!candidate) return null;
    const resolved = resolveRange(candidate);
    if (!resolved) return null;
    const span = resolved.until - resolved.since;
    return { label: rangeLabel(candidate), span };
  })();

  const activeLabel = rangeLabel(value);

  return (
    <div className={styles.root} ref={rootRef}>
      <button
        type="button"
        className={styles.trigger}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Clock size={14} aria-hidden="true" />
        <span className={styles.triggerLabel}>{activeLabel}</span>
      </button>

      {open && (
        <div className={styles.panel} role="dialog" aria-label="Time range">
          <div className={styles.absolute}>
            <div className={styles.sectionTitle}>Absolute or relative</div>

            <label className={styles.field}>
              <span className={styles.fieldLabel}>From</span>
              <input
                ref={firstFieldRef}
                className={styles.input}
                value={from}
                spellCheck={false}
                autoComplete="off"
                onChange={(e) => {
                  setFrom(e.target.value);
                  setError(null);
                }}
                onKeyDown={(e) => e.key === 'Enter' && apply()}
              />
            </label>

            <label className={styles.field}>
              <span className={styles.fieldLabel}>To</span>
              <input
                className={styles.input}
                value={to}
                spellCheck={false}
                autoComplete="off"
                onChange={(e) => {
                  setTo(e.target.value);
                  setError(null);
                }}
                onKeyDown={(e) => e.key === 'Enter' && apply()}
              />
            </label>

            {error ? (
              <div className={styles.error} role="alert">{error}</div>
            ) : preview ? (
              <div className={styles.preview}>
                {preview.label} · {formatSpan(preview.span)}
              </div>
            ) : (
              <div className={styles.preview}>&nbsp;</div>
            )}

            <button type="button" className={styles.apply} onClick={apply}>
              Apply time range
            </button>

            <p className={styles.hint}>
              Supports <code>now</code>, <code>now-24h</code>, <code>now-1d/d</code>. Units:
              s, m, h, d, w, M, y — <code>m</code> is minutes, <code>M</code> is months.
            </p>
          </div>

          <div className={styles.presets}>
            <div className={styles.sectionTitle}>Quick ranges</div>
            <ul className={styles.presetList}>
              {PRESETS.map((p) => {
                const active = p.label === activeLabel;
                return (
                  <li key={p.label}>
                    <button
                      type="button"
                      className={`${styles.preset} ${active ? styles.presetActive : ''}`}
                      aria-current={active || undefined}
                      onClick={() => choosePreset(p.range)}
                    >
                      {p.label}
                    </button>
                  </li>
                );
              })}
            </ul>
          </div>
        </div>
      )}
    </div>
  );
}

/** Compact span for the preview line, e.g. "7d" or "45m". */
function formatSpan(seconds: number): string {
  if (seconds >= 86_400) return `${round(seconds / 86_400)}d`;
  if (seconds >= 3_600) return `${round(seconds / 3_600)}h`;
  if (seconds >= 60) return `${round(seconds / 60)}m`;
  return `${seconds}s`;
}

const round = (n: number) => Math.round(n * 10) / 10;
