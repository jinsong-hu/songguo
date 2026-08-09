// Dashboard building blocks, shared by the Overview and the per-purpose
// Insights pages.
//
// The pages differ only in which sections they mount and what each one breaks
// down by; the sections themselves are identical everywhere. Keeping them here
// is what makes "another Overview, pointed at providers" a twenty-line page
// rather than a second copy of a thousand-line one.

import type { UsageDimension, UsageFilter } from '../../api/types';
import { ErrorBanner } from '../ErrorBanner';
import { Skeleton } from '../Skeleton';
import styles from './dashboard.module.css';

/**
 * Everything a section needs to fetch and label its own series.
 *
 * `filterKey` is a scalar fingerprint of `filter`: useFetch spreads its deps
 * into a useEffect array, so passing the object would refetch on every render.
 */
export interface SeriesScope {
  since: number;
  until: number;
  bucket: string;
  filter: UsageFilter;
  filterKey: string;
  /**
   * Fingerprint of the *selected* range, not the resolved window. A rolling
   * range re-resolves since/until on every tick, so anything that should react
   * to "the user changed the range" — clearing a drilldown selection, say — has
   * to key on this instead, or it fires every ten seconds.
   */
  rangeKey: string;
  /** Auto-refresh cadence; 0 disables polling. */
  intervalMs: number;
  /** Breakdown dimensions this page offers in its segmented controls. */
  dims: DimOption[];
  /** user id -> display name, for the by-user views. Empty for a user key. */
  userNames: Map<string, string>;
}

export interface DimOption {
  key: UsageDimension;
  label: string;
}

/** The full dimension menu. Pages pass a subset. */
export const ALL_DIMS: DimOption[] = [
  { key: 'model', label: 'By model' },
  { key: 'vendor', label: 'By provider' },
  { key: 'user', label: 'By user' },
  { key: 'client', label: 'By client' },
];

/** Pick named dimensions out of ALL_DIMS, preserving the order given. */
export function dims(...keys: UsageDimension[]): DimOption[] {
  return keys
    .map((k) => ALL_DIMS.find((d) => d.key === k))
    .filter((d): d is DimOption => d != null);
}

/**
 * Resolve a series key to what the user should read. Only the by-user view
 * needs the map; every other dimension's key is already the display value.
 */
export function seriesLabeller(
  scope: SeriesScope,
  dim: UsageDimension,
): (key: string) => string {
  return (key: string) => (dim === 'user' ? scope.userNames.get(key) ?? key : key);
}

/**
 * A section's default breakdown, clamped to what the page actually offers. A
 * page that hides the by-user dimension must not be able to open on it.
 */
export function initialDim(scope: SeriesScope, preferred: UsageDimension): UsageDimension {
  return scope.dims.some((d) => d.key === preferred)
    ? preferred
    : scope.dims[0]?.key ?? 'model';
}

interface KpiProps {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub?: React.ReactNode;
  loading?: boolean;
  danger?: boolean;
}

export function Kpi({ icon, label, value, sub, loading, danger }: KpiProps) {
  return (
    <div className={`card ${styles.kpi} ${danger ? styles.kpiDanger : ''}`}>
      <div className={styles.kpiLabel}>
        {icon}
        {label}
      </div>
      {loading ? <Skeleton width={90} height={26} /> : <div className={styles.kpiValue}>{value}</div>}
      {sub && !loading ? <div className={styles.kpiSub}>{sub}</div> : null}
    </div>
  );
}

export function SectionTitle({
  name,
  hint,
  info,
  control,
}: {
  name: string;
  hint?: string;
  info?: React.ReactNode;
  control?: React.ReactNode;
}) {
  return (
    <div className={styles.sectionTitle}>
      <span className={styles.sectionName}>
        {name}
        {info}
      </span>
      {control ?? (hint ? <span className={styles.sectionHint}>{hint}</span> : null)}
    </div>
  );
}

/**
 * The breakdown selector. Rendered as a section's `control`, and omitted
 * entirely when the page offers only one dimension — a segmented control with a
 * single permanently-active button is a label pretending to be a choice.
 */
export function DimensionTabs({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: DimOption[];
  value: UsageDimension;
  onChange: (next: UsageDimension) => void;
}) {
  if (options.length < 2) return null;
  return (
    <div className={styles.seg} role="tablist" aria-label={label}>
      {options.map((d) => (
        <button
          key={d.key}
          role="tab"
          aria-selected={d.key === value}
          className={`${styles.segBtn} ${d.key === value ? styles.segActive : ''}`}
          onClick={() => onChange(d.key)}
        >
          {d.label}
        </button>
      ))}
    </div>
  );
}

export function Panel({
  title,
  caption,
  children,
}: {
  title: string;
  caption?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className={`card ${styles.panel}`}>
      <div className={styles.panelHead}>
        <span className={styles.panelTitle}>{title}</span>
        {caption ? <span className={styles.sectionHint}>{caption}</span> : null}
      </div>
      {children}
    </div>
  );
}

export type FetchLike = { initialLoading: boolean; error: string | null; refetch: () => void };

/** Renders skeleton / error / empty / chart for a useFetch-backed panel body. */
export function Frame({
  r,
  height,
  empty,
  children,
}: {
  r: FetchLike;
  height: string;
  empty: boolean;
  children: React.ReactNode;
}) {
  const inner = r.initialLoading ? (
    <Skeleton height={height ? '100%' : 80} radius={6} />
  ) : r.error ? (
    <ErrorBanner message={r.error} onRetry={r.refetch} />
  ) : empty ? (
    <div className={styles.emptyChart}>No data in this range.</div>
  ) : (
    children
  );
  return height ? <div className={height}>{inner}</div> : <>{inner}</>;
}

export { styles as dashboardStyles };
