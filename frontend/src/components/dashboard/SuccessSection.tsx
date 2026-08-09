import { useEffect, useMemo, useState } from 'react';
import { X } from 'lucide-react';
import { api } from '../../api/client';
import type { ErrorCodeRow, UsageDimension } from '../../api/types';
import { useFetch } from '../../lib/useFetch';
import { bucketLabel, int } from '../../lib/format';
import { outcomeLabel, outcomeStyle, outcomeTone, type Outcome } from '../../lib/outcome';
import { ModelIcon } from '../../lib/modelBrand';
import {
  DimensionTabs,
  Frame,
  Panel,
  SectionTitle,
  dashboardStyles as styles,
  initialDim,
  seriesLabeller,
  type SeriesScope,
} from './Section';

/** What the panel titles and the "All …" chip call the current dimension. */
const NOUN: Record<UsageDimension, { one: string; many: string }> = {
  model: { one: 'By service', many: 'services' },
  vendor: { one: 'By provider', many: 'providers' },
  user: { one: 'By user', many: 'users' },
  client: { one: 'By client', many: 'clients' },
};

/**
 * Success rate per series key, plus the ranked error codes behind it. Clicking a
 * row scopes the error list to it.
 *
 * The rate is over `rated`, not `requests`: songguo's own refusals under a
 * configured limit never reached a provider, so they are in neither side of it
 * (see isPolicyDenial). A bucket can therefore have requests > 0 and rated = 0 —
 * everything refused — which is NOT a clean 100%; it carries rate=null and
 * `denied` so BarStrip can say what actually happened. Buckets with no traffic
 * at all also carry rate=null, with denied=0 to tell the two apart.
 */
export function SuccessSection({
  scope,
  defaultDim = 'model',
  title = 'Success',
}: {
  scope: SeriesScope;
  defaultDim?: UsageDimension;
  title?: string;
}) {
  const [dim, setDim] = useState<UsageDimension>(() => initialDim(scope, defaultDim));
  // Series key of the row the user clicked; scopes the error-codes panel to that
  // one key. Null = all rows (unscoped).
  const [selected, setSelected] = useState<string | null>(null);

  const series = useFetch(
    () => api.successByModel(scope.since, scope.until, scope.bucket, dim, scope.filter),
    [scope.since, scope.until, scope.bucket, dim, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  const rows = useMemo(() => {
    const data = series.data;
    const keys = data?.models ?? [];
    const bucket = data?.bucket ?? scope.bucket;
    const points = data?.points ?? [];
    return keys.map((k) => {
      let totReq = 0;
      let totRated = 0;
      let totDenied = 0;
      let totErr = 0;
      const bars = points.map((p) => {
        const req = p.requests[k] ?? 0;
        const rated = p.rated[k] ?? 0;
        const denied = p.denied[k] ?? 0;
        const err = p.errors[k] ?? 0;
        totReq += req;
        totRated += rated;
        totDenied += denied;
        totErr += err;
        return {
          req,
          denied,
          rate: rated > 0 ? (rated - err) / rated : null,
          label: bucketLabel(p.ts, bucket),
        };
      });
      // Nothing graded → no rate. Showing 100% here would read as "all good" for
      // a row whose every call was refused, which is the opposite of the truth.
      return {
        key: k,
        bars,
        requests: totReq,
        denied: totDenied,
        overall: totRated > 0 ? (totRated - totErr) / totRated : null,
      };
    });
  }, [series.data, scope.bucket]);

  const empty = rows.length === 0;
  // Refusals across every row in view — the panel names them so a "Budget × 946"
  // row in the error-codes panel beside it reconciles with a rate that
  // deliberately ignores those 946 calls.
  const denied = useMemo(() => rows.reduce((n, r) => n + r.denied, 0), [rows]);
  const label = useMemo(() => seriesLabeller(scope, dim), [scope, dim]);

  // "Other" and "unknown" are synthetic buckets, not a real series key, so they
  // can't scope the error-codes panel — treat them as non-selectable.
  const selectableKey = (k: string) => k !== 'Other' && k !== 'unknown';

  // The selection only counts while it names a row currently shown for this
  // dimension/range. Deriving it (rather than reading raw `selected`) means a
  // stale key — from a just-changed dimension, or one that dropped out of the
  // top-N on a live refresh — is ignored immediately, with no wasted mismatched
  // fetch and no filtering to an invisible row. The reset effect below then
  // clears the raw state so the "clear filter" chip disappears too.
  const effective = useMemo(
    () => (selected && rows.some((r) => r.key === selected) ? selected : null),
    [selected, rows],
  );

  const errorCodes = useFetch(
    () => api.errorCodes(scope.since, scope.until, dim, effective ?? undefined, scope.filter),
    [scope.since, scope.until, dim, effective, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  // A selected key is meaningless once the range, dimension, or top-bar filter
  // changes (the key set differs), so clear the raw state. Deriving `effective`
  // already keeps the fetch correct in the same render; this just tidies the
  // stored selection. Keyed on rangeKey, not since/until — see SeriesScope.
  useEffect(() => {
    setSelected(null);
  }, [dim, scope.rangeKey, scope.filterKey]);

  const noun = NOUN[dim] ?? NOUN.model;

  return (
    <>
      <SectionTitle
        name={title}
        control={
          <DimensionTabs
            label="Success breakdown dimension"
            options={scope.dims}
            value={dim}
            onChange={setDim}
          />
        }
      />
      <div className={styles.grid2}>
        <Panel
          title={noun.one}
          // Refusals are in neither side of these percentages, so say so here.
          // Without it the panel is unreconcilable with the "Budget × N" row in
          // Top error codes right beside it, and the rate looks simply wrong.
          caption={denied > 0 ? `${int(denied)} refused · not counted` : undefined}
        >
          <Frame r={series} height="" empty={empty}>
            <div className={styles.svcTable} role="list">
              {rows.map((row) => {
                const selectable = selectableKey(row.key);
                const active = selected === row.key;
                return (
                  <button
                    key={row.key}
                    type="button"
                    role="listitem"
                    className={`${styles.svcRow} ${active ? styles.svcRowActive : ''}`}
                    disabled={!selectable}
                    aria-pressed={active}
                    onClick={() => selectable && setSelected((cur) => (cur === row.key ? null : row.key))}
                    title={selectable ? 'Filter error codes to this row' : undefined}
                  >
                    <span className={styles.svcName}>
                      {dim === 'model' && selectable ? <ModelIcon model={row.key} size={16} /> : null}
                      <span className={styles.svcLabel}>{label(row.key)}</span>
                    </span>
                    <BarStrip bars={row.bars} />
                    <span className={styles.rowPct} style={{ color: bandColor(row.overall) }}>
                      {row.overall == null ? '—' : `${Math.round(row.overall * 100)}%`}
                    </span>
                  </button>
                );
              })}
            </div>
          </Frame>
        </Panel>

        <Panel title="Top error codes">
          <div className={styles.ecFilter}>
            {effective ? (
              <button type="button" className={styles.ecClear} onClick={() => setSelected(null)}>
                <span className={styles.ecFilterName}>{label(effective)}</span>
                <X size={12} />
              </button>
            ) : (
              <span className={styles.ecFilterAll}>All {noun.many}</span>
            )}
          </div>
          <Frame r={errorCodes} height="" empty={(errorCodes.data?.rows.length ?? 0) === 0}>
            <ErrorCodeList rows={errorCodes.data?.rows ?? []} />
          </Frame>
        </Panel>
      </div>
    </>
  );
}

/**
 * Success-rate bands, best first: 99–100 · 95–99 · 90–95 · 80–90 · 60–80 · <60.
 *
 * Colour and bar height both come out of this one table, so the two encodings
 * can never drift apart — greener is always taller. Height is banded rather than
 * linear because everything worth seeing lives in the top few percent: on a
 * linear scale a 95% bucket and a perfect one draw as the same full bar, and the
 * strip reads as uniformly fine. Six steps of 16% make the difference obvious at
 * a 34px strip height.
 */
const BANDS: { min: number; color: string; height: number }[] = [
  { min: 0.99, color: 'var(--band-6)', height: 100 },
  { min: 0.95, color: 'var(--band-5)', height: 84 },
  { min: 0.9, color: 'var(--band-4)', height: 68 },
  { min: 0.8, color: 'var(--band-3)', height: 52 },
  { min: 0.6, color: 'var(--band-2)', height: 36 },
  { min: 0, color: 'var(--band-1)', height: 20 },
];

function band(rate: number): { color: string; height: number } {
  return BANDS.find((b) => rate >= b.min) ?? BANDS[BANDS.length - 1];
}

// Bars and the overall % share the band scale; no traffic → muted grey.
function bandColor(rate: number | null): string {
  return rate == null ? 'var(--text-muted)' : band(rate).color;
}

interface SuccessBar {
  req: number;
  /** Calls songguo refused under a configured limit — graded in neither side. */
  denied: number;
  /** null when nothing in this bucket was graded: no traffic, or all refused. */
  rate: number | null;
  label: string;
}

/**
 * A compact strip of vertical bars, one per time bucket. Height AND color both
 * encode the bucket's success rate on the same fixed band scale — a short bar
 * always means a bad bucket, regardless of the current view or traffic.
 * No-traffic buckets (rate=null) count as a clean 100% — nothing failed — so
 * they render a full-height green bar rather than a gap; the worst band still
 * stands 20% tall so a 0%-ok bar is a visible red stub.
 *
 * A bucket whose calls were all refused also has no rate, but it is NOT the same
 * as no traffic and must not borrow that green bar: it gets a full-height muted
 * one, which reads as "nothing to grade here" rather than "all good".
 */
function BarStrip({ bars }: { bars: SuccessBar[] }) {
  return (
    <div className={styles.barStrip} aria-hidden="true">
      {bars.map((b, i) => {
        const refusedOnly = b.rate == null && b.denied > 0;
        const { color, height } = band(b.rate == null ? 1 : b.rate);
        return (
          <div key={i} className={styles.barSlot}>
            <div
              className={styles.bar}
              style={{
                height: `${height}%`,
                background: refusedOnly ? 'var(--text-muted)' : color,
              }}
              title={
                refusedOnly
                  ? `${b.label}: ${b.denied} refused · nothing to grade`
                  : b.rate == null
                    ? `${b.label}: no requests`
                    : `${b.label}: ${Math.round(b.rate * 100)}% ok · ${b.req} req` +
                      (b.denied > 0 ? ` · ${b.denied} refused` : '')
              }
            />
          </div>
        );
      })}
    </div>
  );
}

// Human label for a ranked failure row. Keyed on the outcome, not the status —
// grouping by the integer merged songguo's own 429 with the provider's, and the
// four distinct causes of a 502 into one row. `vendor_error` has no fixed label
// because there the status IS the answer, so it reads by class.
function errorReason(row: ErrorCodeRow): string {
  if (row.outcome === 'vendor_error' || row.outcome === '') {
    if (row.status === 429) return 'Provider rate-limited';
    if (row.status >= 500) return 'Provider server error';
    if (row.status >= 400) return 'Provider rejected';
    return outcomeStyle('unknown').label;
  }
  return outcomeStyle(row.outcome as Outcome).hint;
}

// Pill/proportion-bar color, from the shared tone scale so this list cannot
// disagree with the pill rendered on the request log.
function errorColor(row: ErrorCodeRow): string {
  switch (outcomeTone((row.outcome || 'vendor_error') as Outcome, row.status)) {
    case 'ok':
      return 'var(--accent)';
    case 'warn':
      return 'var(--amber)';
    case 'gateway':
      return 'var(--violet)';
    case 'idle':
      return 'var(--text-muted)';
    default:
      return 'var(--danger)';
  }
}

/** Ranked list of top error codes with a status pill, reason, count, and a
 *  proportion bar (relative to the largest count). */
function ErrorCodeList({ rows }: { rows: ErrorCodeRow[] }) {
  const max = rows.reduce((m, r) => Math.max(m, r.count), 0);
  return (
    <div className={styles.ecList} role="list">
      {rows.map((r) => {
        const color = errorColor(r);
        return (
          <div key={`${r.status}:${r.outcome}`} className={styles.ecRow} role="listitem">
            <span className={styles.ecStatus} style={{ color, borderColor: color }}>
              {outcomeLabel((r.outcome || 'vendor_error') as Outcome, r.status)}
            </span>
            <span className={styles.ecReason} title={errorReason(r)}>
              {errorReason(r)}
            </span>
            <div className={styles.ecBarTrack}>
              <div
                className={styles.ecBar}
                style={{ width: `${max > 0 ? (r.count / max) * 100 : 0}%`, background: color }}
              />
            </div>
            <span className={styles.ecCount}>{int(r.count)}</span>
          </div>
        );
      })}
    </div>
  );
}
