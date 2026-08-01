import { useEffect, useMemo, useRef, useState } from 'react';
import { Check, ChevronDown } from 'lucide-react';
import type { Facet } from '../api/types';
import styles from './FacetSelect.module.css';

export interface FacetSelectProps {
  /** Plural noun for the whole set, e.g. "models". Drives the all-state label
   *  ("All models"), the count label ("3 models"), and the panel's aria-label.
   *  Only ever plural — a lone selection shows its own name instead of a count,
   *  so there is no "1 model" case needing a singular. */
  label: string;
  /** Options, already ranked by the backend (most requests first). */
  options: Facet[];
  /** Selected keys. Empty = all, which is the default and the reset target. */
  value: string[];
  onChange: (next: string[]) => void;
  /** Optional leading glyph per option (a model or provider brand icon). It is
   *  drawn in a fixed-width slot, so returning null for an unrecognized brand
   *  leaves the names aligned rather than shifting that row left. */
  renderIcon?: (key: string) => React.ReactNode;
  /** Optional prettier display name; the raw key is still what we filter on. */
  renderLabel?: (key: string) => string;
  /** Show the search box once the list is at least this long. */
  searchThreshold?: number;
}

/**
 * A toolbar multi-select: a TimeRangePicker-shaped trigger that opens a panel of
 * checkboxes. Sits beside the time range and narrows the whole page the same way
 * it does.
 *
 * Two conventions carried from elsewhere in the app:
 *
 * - **Empty means all.** Not "nothing" — the same reading `User.scope` uses for
 *   model allowlists, and the reason the client can simply omit the param. It
 *   also means there is no way to select an empty result set by accident:
 *   unchecking the last box returns you to All rather than to a blank dashboard.
 * - **A selected key that has left the option list still shows.** The options
 *   come from traffic in the current time range, so narrowing the range can drop
 *   one out from under an active selection. Those are appended as zero-request
 *   rows rather than silently dropped, because a filter you cannot see is a
 *   filter you cannot clear.
 */
export function FacetSelect({
  label,
  options,
  value,
  onChange,
  renderIcon,
  renderLabel,
  searchThreshold = 8,
}: FacetSelectProps) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const rootRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  // Options actually seen in the window, plus any selection that has fallen out
  // of them (see the class comment). Zero-request strays sort last.
  const rows = useMemo(() => {
    const known = new Set(options.map((o) => o.key));
    const strays = value.filter((k) => !known.has(k)).map((key) => ({ key, requests: 0 }));
    return [...options, ...strays];
  }, [options, value]);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(
      (r) =>
        r.key.toLowerCase().includes(q) ||
        (renderLabel?.(r.key) ?? '').toLowerCase().includes(q),
    );
  }, [rows, query, renderLabel]);

  // Reseed on open so an abandoned search never greets the next opening.
  useEffect(() => {
    if (!open) return;
    setQuery('');
    searchRef.current?.focus();
  }, [open]);

  // Dismiss on outside pointerdown or Escape, mirroring TimeRangePicker: the
  // listeners are on the document so Escape works wherever focus has wandered.
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

  const toggle = (key: string) => {
    onChange(value.includes(key) ? value.filter((k) => k !== key) : [...value, key]);
  };

  // The panel stays open across toggles: picking several is the whole point of a
  // multi-select, and closing after each one would make that four round trips.
  const all = value.length === 0;
  const triggerLabel = all
    ? `All ${label}`
    : value.length === 1
      ? (renderLabel?.(value[0]) ?? value[0])
      : `${value.length} ${label}`;

  const showSearch = rows.length >= searchThreshold;

  return (
    <div className={styles.root} ref={rootRef}>
      <button
        type="button"
        className={`${styles.trigger} ${all ? '' : styles.triggerActive}`}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        title={all ? undefined : value.join(', ')}
      >
        <span className={styles.triggerLabel}>{triggerLabel}</span>
        <ChevronDown size={13} aria-hidden="true" />
      </button>

      {open && (
        <div className={styles.panel} role="dialog" aria-label={label}>
          <div className={styles.head}>
            {showSearch && (
              <input
                ref={searchRef}
                className={styles.search}
                value={query}
                placeholder={`Filter ${label}…`}
                spellCheck={false}
                autoComplete="off"
                aria-label={`Filter ${label}`}
                onChange={(e) => setQuery(e.target.value)}
              />
            )}
            <button
              type="button"
              className={styles.clear}
              disabled={all}
              onClick={() => onChange([])}
            >
              All {label}
            </button>
          </div>

          <ul className={styles.list}>
            {visible.length === 0 && <li className={styles.empty}>No {label} match.</li>}
            {visible.map((row) => {
              const checked = value.includes(row.key);
              return (
                <li key={row.key}>
                  <label className={styles.opt}>
                    <input
                      type="checkbox"
                      className={styles.check}
                      checked={checked}
                      onChange={() => toggle(row.key)}
                    />
                    <span className={styles.box} aria-hidden="true">
                      {checked ? <Check size={11} strokeWidth={3} /> : null}
                    </span>
                    {renderIcon ? (
                      <span className={styles.icon}>{renderIcon(row.key)}</span>
                    ) : null}
                    <span className={styles.name}>{renderLabel?.(row.key) ?? row.key}</span>
                    <span className={styles.count}>
                      {row.requests > 0 ? compact(row.requests) : '—'}
                    </span>
                  </label>
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}

/** Compact a request count for the narrow right-hand column. */
function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}
