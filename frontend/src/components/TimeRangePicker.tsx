import { useEffect, useMemo, useRef, useState } from 'react';
import { Clock } from 'lucide-react';
import { DayPicker } from 'react-day-picker';
import rdp from 'react-day-picker/style.module.css';
import {
  PRESETS,
  type TimeRange,
  calendarBounds,
  calendarSelection,
  rangeFromCalendar,
  rangeFromInputs,
  rangeLabel,
  rangeToInputs,
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

  const activeLabel = rangeLabel(value);

  // Take react-day-picker's own layout classes and append ours, rather than
  // replacing them — the library's grid geometry is worth keeping.
  const calendarClassNames = useMemo(
    () => ({
      ...rdp,
      // The theme variables ride on the root, so this one is not cosmetic —
      // without it the grid keeps react-day-picker's default blue accent.
      root: `${rdp.root} ${styles.calendarRoot}`,
      weekday: `${rdp.weekday} ${styles.calendarWeekday}`,
      caption_label: `${rdp.caption_label} ${styles.calendarCaption}`,
      day_button: `${rdp.day_button} ${styles.calendarDayButton}`,
      range_middle: `${rdp.range_middle} ${styles.calendarMiddle}`,
      button_next: `${rdp.button_next} ${styles.calendarNav}`,
      button_previous: `${rdp.button_previous} ${styles.calendarNav}`,
    }),
    [],
  );

  // Recomputed from the fields, so the grid shades whatever they currently mean
  // — including a relative expression's implied window.
  const selected = useMemo(() => calendarSelection(from, to), [from, to]);
  const bounds = useMemo(() => calendarBounds(), []);

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

            <div className={styles.calendar}>
              <DayPicker
                mode="range"
                classNames={calendarClassNames}
                selected={selected}
                defaultMonth={selected?.from}
                startMonth={bounds.fromDate}
                endMonth={bounds.toDate}
                disabled={{ before: bounds.fromDate, after: bounds.toDate }}
                showOutsideDays
                onSelect={(next) => {
                  if (!next?.from) return;
                  // Writing back into the fields keeps them the single source of
                  // truth; the grid is an input method, not a parallel state.
                  const picked = rangeFromCalendar(next.from, next.to);
                  setFrom(picked.from);
                  setTo(picked.to);
                  setError(null);
                }}
              />
            </div>

            {error && (
              <div className={styles.error} role="alert">{error}</div>
            )}

            <button type="button" className={styles.apply} onClick={apply}>
              Apply time range
            </button>
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
