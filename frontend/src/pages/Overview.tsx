import { useEffect, useMemo, useState } from 'react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  XAxis,
  YAxis,
} from 'recharts';
import { Activity, Clock, Coins, DatabaseZap, DollarSign, GitBranch, MessageSquare, ShieldCheck, Users, Wrench, X } from 'lucide-react';
import { api } from '../api/client';
import type { Bucket, ErrorCodeRow, UsageDimension } from '../api/types';
import { ContextSunburst, ContextDistributionCard } from '../components/ContextSunburst';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '../components/ui/chart';
import { LIVE_REFRESH_MS, useFetch, useLiveTick } from '../lib/useFetch';
import { bucketLabel, duration, int, money, percent } from '../lib/format';
import { brandOf, providerBrand, ModelIcon } from '../lib/modelBrand';
import { ActivityFeed } from './ActivityFeed';
import styles from './Overview.module.css';

interface RangeOption {
  key: string;
  label: string;
  seconds: number;
  bucket: Bucket;
}

const RANGES: RangeOption[] = [
  { key: '24h', label: '24h', seconds: 24 * 3600, bucket: 'hour' },
  { key: '7d', label: '7d', seconds: 7 * 24 * 3600, bucket: 'day' },
  { key: '30d', label: '30d', seconds: 30 * 24 * 3600, bucket: 'day' },
];

const REFRESH_MS = LIVE_REFRESH_MS;

// Usage stacked-chart breakdown dimensions. "provider" is the label for the
// vendor dimension (the calls `vendor` column) — see UsageDimension.
const USAGE_DIMS: { key: UsageDimension; label: string }[] = [
  { key: 'model', label: 'By model' },
  { key: 'vendor', label: 'By provider' },
  { key: 'user', label: 'By user' },
];
const CHART_CLS = 'aspect-auto h-full w-full';

type FetchLike = { initialLoading: boolean; error: string | null; refetch: () => void };

export function OverviewPage() {
  const [rangeKey, setRangeKey] = useState('24h');
  const [usageDim, setUsageDim] = useState<UsageDimension>('model');
  const [perfDim, setPerfDim] = useState<UsageDimension>('model');
  const [successDim, setSuccessDim] = useState<UsageDimension>('model');
  const [cacheDim, setCacheDim] = useState<UsageDimension>('model');
  // Series key of the Success row the user clicked; scopes the error-codes panel
  // to that one service/vendor/user. Null = all rows (unscoped).
  const [selectedService, setSelectedService] = useState<string | null>(null);
  const tick = useLiveTick(REFRESH_MS);
  const range = RANGES.find((r) => r.key === rangeKey) ?? RANGES[0];

  const { since, until } = useMemo(() => {
    const u = tick + 1;
    return { since: u - range.seconds, until: u };
  }, [tick, range]);

  const opts = { intervalMs: REFRESH_MS };
  const overview = useFetch(() => api.overview(since, until), [since, until], opts);
  const sessions = useFetch(() => api.sessionsOverview(since, until), [since, until], opts);
  const tokenSeries = useFetch(
    () => api.tokensByModel(since, until, range.bucket, usageDim),
    [since, until, range.bucket, usageDim],
    opts,
  );
  // Performance charts reuse the tokens-by-model endpoint (which also carries
  // per-key TTFT/throughput) with an independent dimension selector.
  const perfSeries = useFetch(
    () => api.tokensByModel(since, until, range.bucket, perfDim),
    [since, until, range.bucket, perfDim],
    opts,
  );
  const successSeries = useFetch(
    () => api.successByModel(since, until, range.bucket, successDim),
    [since, until, range.bucket, successDim],
    opts,
  );
  const cacheSeries = useFetch(
    () => api.cacheByModel(since, until, range.bucket, cacheDim),
    [since, until, range.bucket, cacheDim],
    opts,
  );
  // Resolve user-id series keys to display names for the by-user views.
  const usersList = useFetch(() => api.users(), [], opts);
  const composition = useFetch(() => api.contextComposition(since, until), [since, until], opts);

  const ov = overview.data;
  const ss = sessions.data;

  // Overall cache-hit ratio: cache reads / total input (fresh + cache read + cache
  // write). Null when there was no input in range, so the KPI shows "—" rather than
  // a misleading 0%.
  const cacheHit = useMemo(() => {
    if (!ov) return null;
    const totalInput = ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation;
    return totalInput > 0 ? ov.tokens.cached / totalInput : null;
  }, [ov]);

  // Tokens-by-model chart: one row per bucket with a numeric column per model.
  // Bars stack the model columns. Models come pre-ranked from the backend
  // (top N + "Other").
  const { tokenModels, tokenPoints, costPoints } = useMemo(() => {
    const data = tokenSeries.data;
    const modelKeys = data?.models ?? [];
    const bucket = data?.bucket ?? range.bucket;
    const tokRows: Record<string, number | string>[] = [];
    const costRows: Record<string, number | string>[] = [];
    for (const p of data?.points ?? []) {
      const label = bucketLabel(p.ts, bucket);
      const tok: Record<string, number | string> = { label };
      const cost: Record<string, number | string> = { label };
      for (const m of modelKeys) {
        tok[m] = p.tokens[m] ?? 0;
        cost[m] = p.costs[m] ?? 0;
      }
      tokRows.push(tok);
      costRows.push(cost);
    }
    return { tokenModels: modelKeys, tokenPoints: tokRows, costPoints: costRows };
  }, [tokenSeries.data, range.bucket]);
  // Map a series key to its display label: user ids resolve to names in the
  // by-user view (falling back to the id); other dimensions show the key as-is.
  const seriesLabel = useMemo(() => {
    const names = new Map((usersList.data ?? []).map((u) => [u.id, u.name]));
    return (key: string) => (usageDim === 'user' ? names.get(key) ?? key : key);
  }, [usersList.data, usageDim]);
  const tokenConfig = useMemo<ChartConfig>(() => {
    const c: ChartConfig = {};
    tokenModels.forEach((m) => {
      c[m] = { label: seriesLabel(m) };
    });
    return c;
  }, [tokenModels, seriesLabel]);
  // Distinct, brand-anchored color per series key for the current dimension.
  const seriesColors = useMemo(
    () => assignSeriesColors(tokenModels, usageDim),
    [tokenModels, usageDim],
  );

  // Performance-by-dimension: one row per bucket with a numeric TTFT and a
  // numeric throughput column per key. Mirrors the tokens-by-model shaping but
  // reads the ttft/tps maps and uses the independent perfDim selector.
  const { perfKeys, ttftPoints, tpsPoints } = useMemo(() => {
    const data = perfSeries.data;
    const keys = data?.models ?? [];
    const bucket = data?.bucket ?? range.bucket;
    const ttftRows: Record<string, number | string>[] = [];
    const tpsRows: Record<string, number | string>[] = [];
    for (const p of data?.points ?? []) {
      const label = bucketLabel(p.ts, bucket);
      const ttft: Record<string, number | string> = { label };
      const tps: Record<string, number | string> = { label };
      for (const k of keys) {
        ttft[k] = p.ttft[k] ?? 0;
        tps[k] = p.tps[k] ?? 0;
      }
      ttftRows.push(ttft);
      tpsRows.push(tps);
    }
    return { perfKeys: keys, ttftPoints: ttftRows, tpsPoints: tpsRows };
  }, [perfSeries.data, range.bucket]);
  const perfLabel = useMemo(() => {
    const names = new Map((usersList.data ?? []).map((u) => [u.id, u.name]));
    return (key: string) => (perfDim === 'user' ? names.get(key) ?? key : key);
  }, [usersList.data, perfDim]);
  const perfConfig = useMemo<ChartConfig>(() => {
    const c: ChartConfig = {};
    perfKeys.forEach((k) => {
      c[k] = { label: perfLabel(k) };
    });
    return c;
  }, [perfKeys, perfLabel]);
  const perfColors = useMemo(() => assignSeriesColors(perfKeys, perfDim), [perfKeys, perfDim]);
  const perfEmpty = perfKeys.length === 0;

  // Success bar-table: one entry per series key (pre-ranked by request volume by
  // the backend, top N + "Other"), each carrying a per-bucket bar (request volume
  // + success rate) and an overall success %. Bar height AND color both encode the
  // bucket's success rate, so a short bar always means a bad bucket regardless of
  // the current view. Buckets with no requests carry rate=null (rendered as a flat
  // baseline tick so bars stay time-aligned across rows).
  const { successRows } = useMemo(() => {
    const data = successSeries.data;
    const keys = data?.models ?? [];
    const bucket = data?.bucket ?? range.bucket;
    const points = data?.points ?? [];
    const rows = keys.map((k) => {
      let totReq = 0;
      let totErr = 0;
      const bars = points.map((p) => {
        const req = p.requests[k] ?? 0;
        const err = p.errors[k] ?? 0;
        totReq += req;
        totErr += err;
        return { req, rate: req > 0 ? (req - err) / req : null, label: bucketLabel(p.ts, bucket) };
      });
      // Zero volume → nothing failed, so show a clean 100% rather than an
      // empty "—" that reads like a problem.
      return { key: k, bars, requests: totReq, overall: totReq > 0 ? (totReq - totErr) / totReq : 1 };
    });
    return { successRows: rows };
  }, [successSeries.data, range.bucket]);
  const successEmpty = successRows.length === 0;
  // Series-key -> display label for the Success dimension (user ids resolve to
  // names in the by-user view, like seriesLabel does for Usage).
  const successSeriesLabel = useMemo(() => {
    const names = new Map((usersList.data ?? []).map((u) => [u.id, u.name]));
    return (key: string) => (successDim === 'user' ? names.get(key) ?? key : key);
  }, [usersList.data, successDim]);
  // "Other" and "unknown" are synthetic buckets, not a real series key, so they
  // can't scope the error-codes panel — treat them as non-selectable.
  const selectableKey = (k: string) => k !== 'Other' && k !== 'unknown';
  // The selection only counts while it names a row currently shown for this
  // dimension/range. Deriving it (rather than reading raw selectedService) means a
  // stale key — from a just-changed dimension, or one that dropped out of the
  // top-N on a live refresh — is ignored immediately, with no wasted mismatched
  // fetch and no filtering to an invisible row. The reset effect below then clears
  // the raw state so the "clear filter" chip disappears too.
  const effectiveService = useMemo(
    () =>
      selectedService && successRows.some((r) => r.key === selectedService)
        ? selectedService
        : null,
    [selectedService, successRows],
  );
  // Top error codes, scoped to the effective Success row (dimension + key) or all
  // rows when none is selected. Re-fetches when the effective selection changes.
  const errorCodes = useFetch(
    () => api.errorCodes(since, until, successDim, effectiveService ?? undefined),
    [since, until, successDim, effectiveService],
    opts,
  );
  // A selected key is meaningless once the range or dimension changes (the key
  // set differs), so clear the raw state. Deriving effectiveService already keeps
  // the fetch correct in the same render; this just tidies the stored selection.
  useEffect(() => {
    setSelectedService(null);
  }, [successDim, rangeKey]);

  // Cache-hit %-over-time chart: one row per bucket with a per-key cache-hit rate
  // (cache reads / total input tokens, as a %). Null when the key had no input in
  // that bucket, so the line breaks rather than dropping to 0%. Keys come pre-ranked
  // by total input from the backend (top N + "Other").
  const { cacheModels, cachePoints } = useMemo(() => {
    const data = cacheSeries.data;
    const keys = data?.models ?? [];
    const bucket = data?.bucket ?? range.bucket;
    const rows: Record<string, number | string | null>[] = [];
    for (const p of data?.points ?? []) {
      const row: Record<string, number | string | null> = { label: bucketLabel(p.ts, bucket) };
      for (const k of keys) {
        const input = p.input[k] ?? 0;
        const cacheRead = p.cache_read[k] ?? 0;
        row[k] = input > 0 ? (cacheRead / input) * 100 : null;
      }
      rows.push(row);
    }
    return { cacheModels: keys, cachePoints: rows };
  }, [cacheSeries.data, range.bucket]);
  const cacheEmpty = cacheModels.length === 0;
  const cacheSeriesLabel = useMemo(() => {
    const names = new Map((usersList.data ?? []).map((u) => [u.id, u.name]));
    return (key: string) => (cacheDim === 'user' ? names.get(key) ?? key : key);
  }, [usersList.data, cacheDim]);
  const cacheConfig = useMemo<ChartConfig>(() => {
    const c: ChartConfig = {};
    cacheModels.forEach((m) => {
      c[m] = { label: cacheSeriesLabel(m) };
    });
    return c;
  }, [cacheModels, cacheSeriesLabel]);
  const cacheColors = useMemo(
    () => assignSeriesColors(cacheModels, cacheDim),
    [cacheModels, cacheDim],
  );

  const rangeSwitch = (
    <div className={styles.seg} role="tablist" aria-label="Time range">
      {RANGES.map((r) => (
        <button
          key={r.key}
          role="tab"
          aria-selected={r.key === rangeKey}
          className={`${styles.segBtn} ${r.key === rangeKey ? styles.segActive : ''}`}
          onClick={() => setRangeKey(r.key)}
        >
          {r.label}
        </button>
      ))}
    </div>
  );

  return (
    <Page title="Overview" actions={rangeSwitch}>
      {overview.error && (
        <div style={{ marginBottom: 16 }}>
          <ErrorBanner message={overview.error} onRetry={overview.refetch} />
        </div>
      )}

      {/* KPI cards */}
      <div className={styles.kpiGrid}>
        <Kpi
          icon={<Activity size={14} />}
          label="Sessions"
          loading={overview.initialLoading || sessions.initialLoading}
          value={ss ? int(ss.sessions) : '—'}
          sub={ov ? `${int(ov.requests)} calls` : undefined}
        />
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens"
          loading={overview.initialLoading}
          value={ov ? int(ov.tokens.input + ov.tokens.output) : '—'}
          sub={
            ov ? (
              <span className={styles.kpiSubSplit}>
                <span>{int(ov.tokens.input)} in</span>
                <span>{int(ov.tokens.output)} out</span>
              </span>
            ) : undefined
          }
        />
        <Kpi
          icon={<DollarSign size={14} />}
          label="Cost"
          loading={overview.initialLoading}
          value={ov ? money(ov.total_spend) : '—'}
        />
        <Kpi
          icon={<Users size={14} />}
          label="Active users"
          loading={overview.initialLoading}
          value={ov ? int(ov.active_callers) : '—'}
        />
        <Kpi
          icon={<ShieldCheck size={14} />}
          label="Success rate"
          loading={overview.initialLoading}
          value={ov ? percent(1 - ov.error_rate) : '—'}
          danger={ov != null && ov.error_rate > 0.05}
        />
        <Kpi
          icon={<DatabaseZap size={14} />}
          label="Cache hit"
          loading={overview.initialLoading}
          value={cacheHit == null ? '—' : percent(cacheHit)}
        />
      </div>

      {/* Usage */}
      <SectionTitle
        name="Usage"
        control={
          <div className={styles.seg} role="tablist" aria-label="Usage breakdown dimension">
            {USAGE_DIMS.map((d) => (
              <button
                key={d.key}
                role="tab"
                aria-selected={d.key === usageDim}
                className={`${styles.segBtn} ${d.key === usageDim ? styles.segActive : ''}`}
                onClick={() => setUsageDim(d.key)}
              >
                {d.label}
              </button>
            ))}
          </div>
        }
      />
      <div className={styles.grid2}>
        <Panel title="Tokens">
          <Frame r={tokenSeries} height={styles.chartSm} empty={tokenModels.length === 0}>
            <ChartContainer config={tokenConfig} className={CHART_CLS}>
              <BarChart data={tokenPoints} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {tokenModels.map((m, i) => (
                  <Bar
                    key={m}
                    dataKey={m}
                    stackId="tok"
                    fill={seriesColors[m]}
                    radius={i === tokenModels.length - 1 ? [3, 3, 0, 0] : undefined}
                  />
                ))}
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Cost">
          <Frame r={tokenSeries} height={styles.chartSm} empty={tokenModels.length === 0}>
            <ChartContainer config={tokenConfig} className={CHART_CLS}>
              <BarChart data={costPoints} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={52} tickFormatter={(v: number) => money(v)} />
                <ChartTooltip
                  content={
                    <ChartTooltipContent
                      formatter={(value, name, item) => (
                        <div className={styles.costTip}>
                          <span
                            className={styles.costTipDot}
                            style={{ background: (item?.color as string) ?? 'var(--text-muted)' }}
                          />
                          <span className={styles.costTipName}>{seriesLabel(String(name))}</span>
                          <span className={styles.costTipVal}>{money(Number(value))}</span>
                        </div>
                      )}
                    />
                  }
                />
                <ChartLegend content={<ChartLegendContent />} />
                {tokenModels.map((m, i) => (
                  <Bar
                    key={m}
                    dataKey={m}
                    stackId="cost"
                    fill={seriesColors[m]}
                    radius={i === tokenModels.length - 1 ? [3, 3, 0, 0] : undefined}
                  />
                ))}
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>

      {/* Context distribution */}
      <ContextDistributionCard>
        <Frame r={composition} height="" empty={(composition.data?.sources.length ?? 0) === 0}>
          {composition.data ? <ContextSunburst data={composition.data} /> : null}
        </Frame>
      </ContextDistributionCard>

      {/* Performance */}
      <SectionTitle
        name="Performance"
        control={
          <div className={styles.seg} role="tablist" aria-label="Performance breakdown dimension">
            {USAGE_DIMS.map((d) => (
              <button
                key={d.key}
                role="tab"
                aria-selected={d.key === perfDim}
                className={`${styles.segBtn} ${d.key === perfDim ? styles.segActive : ''}`}
                onClick={() => setPerfDim(d.key)}
              >
                {d.label}
              </button>
            ))}
          </div>
        }
      />
      <div className={styles.grid2}>
        <Panel title="Avg TTFT">
          <Frame r={perfSeries} height={styles.chartXs} empty={perfEmpty}>
            <ChartContainer config={perfConfig} className={CHART_CLS}>
              <LineChart data={ttftPoints} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => `${Math.round(v)}`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {perfKeys.map((k) => (
                  <Line key={k} dataKey={k} type="monotone" stroke={perfColors[k]} strokeWidth={2} dot={false} connectNulls />
                ))}
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Avg throughput">
          <Frame r={perfSeries} height={styles.chartXs} empty={perfEmpty}>
            <ChartContainer config={perfConfig} className={CHART_CLS}>
              <LineChart data={tpsPoints} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {perfKeys.map((k) => (
                  <Line key={k} dataKey={k} type="monotone" stroke={perfColors[k]} strokeWidth={2} dot={false} connectNulls />
                ))}
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>

      {/* Success */}
      <SectionTitle
        name="Success"
        control={
          <div className={styles.seg} role="tablist" aria-label="Success breakdown dimension">
            {USAGE_DIMS.map((d) => (
              <button
                key={d.key}
                role="tab"
                aria-selected={d.key === successDim}
                className={`${styles.segBtn} ${d.key === successDim ? styles.segActive : ''}`}
                onClick={() => setSuccessDim(d.key)}
              >
                {d.label}
              </button>
            ))}
          </div>
        }
      />
      <div className={styles.grid2}>
        <Panel
          title={successDim === 'vendor' ? 'By provider' : successDim === 'user' ? 'By user' : 'By service'}
        >
          <Frame r={successSeries} height="" empty={successEmpty}>
            <div className={styles.svcTable} role="list">
              {successRows.map((row) => {
                const selectable = selectableKey(row.key);
                const active = selectedService === row.key;
                return (
                  <button
                    key={row.key}
                    type="button"
                    role="listitem"
                    className={`${styles.svcRow} ${active ? styles.svcRowActive : ''}`}
                    disabled={!selectable}
                    aria-pressed={active}
                    onClick={() =>
                      selectable && setSelectedService((cur) => (cur === row.key ? null : row.key))
                    }
                    title={selectable ? 'Filter error codes to this row' : undefined}
                  >
                    <span className={styles.svcName}>
                      {successDim === 'model' && selectable ? (
                        <ModelIcon model={row.key} size={16} />
                      ) : null}
                      <span className={styles.svcLabel}>{successSeriesLabel(row.key)}</span>
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
            {effectiveService ? (
              <button type="button" className={styles.ecClear} onClick={() => setSelectedService(null)}>
                <span className={styles.ecFilterName}>{successSeriesLabel(effectiveService)}</span>
                <X size={12} />
              </button>
            ) : (
              <span className={styles.ecFilterAll}>All {successDim === 'vendor' ? 'providers' : successDim === 'user' ? 'users' : 'services'}</span>
            )}
          </div>
          <Frame r={errorCodes} height="" empty={(errorCodes.data?.rows.length ?? 0) === 0}>
            <ErrorCodeList rows={errorCodes.data?.rows ?? []} />
          </Frame>
        </Panel>
      </div>

      {/* Cache */}
      <SectionTitle
        name="Cache"
        control={
          <div className={styles.seg} role="tablist" aria-label="Cache breakdown dimension">
            {USAGE_DIMS.map((d) => (
              <button
                key={d.key}
                role="tab"
                aria-selected={d.key === cacheDim}
                className={`${styles.segBtn} ${d.key === cacheDim ? styles.segActive : ''}`}
                onClick={() => setCacheDim(d.key)}
              >
                {d.label}
              </button>
            ))}
          </div>
        }
      />
      <Panel title="Cache hit rate">
        <Frame r={cacheSeries} height={styles.chartSm} empty={cacheEmpty}>
          <ChartContainer config={cacheConfig} className={CHART_CLS}>
            <LineChart data={cachePoints} margin={{ top: 6, right: 8, left: -16, bottom: 0 }}>
              <CartesianGrid vertical={false} />
              <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
              <YAxis tickLine={false} axisLine={false} width={40} domain={[0, 100]} tickFormatter={(v: number) => `${v}%`} />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    formatter={(value, name, item) => (
                      <div className={styles.costTip}>
                        <span
                          className={styles.costTipDot}
                          style={{ background: (item?.color as string) ?? 'var(--text-muted)' }}
                        />
                        <span className={styles.costTipName}>{cacheSeriesLabel(String(name))}</span>
                        <span className={styles.costTipVal}>
                          {value == null ? '—' : `${Number(value).toFixed(1)}%`}
                        </span>
                      </div>
                    )}
                  />
                }
              />
              <ChartLegend content={<ChartLegendContent />} />
              {cacheModels.map((m) => (
                <Line
                  key={m}
                  dataKey={m}
                  type="monotone"
                  stroke={cacheColors[m]}
                  strokeWidth={2}
                  dot={false}
                  connectNulls
                />
              ))}
            </LineChart>
          </ChartContainer>
        </Frame>
      </Panel>

      {/* Behavioral */}
      <SectionTitle name="Behavioral" hint="How agent runs behave — per-session turns, duration, tokens, and tool use" />
      <div className={styles.kpiGrid}>
        <Kpi
          icon={<GitBranch size={14} />}
          label={`Sessions (${range.label})`}
          loading={sessions.initialLoading}
          value={ss ? int(ss.sessions) : '—'}
          sub={ss ? `${int(ss.with_subagents)} used subagents` : undefined}
        />
        <Kpi
          icon={<MessageSquare size={14} />}
          label="Turns / session"
          loading={sessions.initialLoading}
          value={ss ? ss.avg_turns.toFixed(1) : '—'}
          sub={ss ? `p50 ${int(ss.turns_p50)} · p95 ${int(ss.turns_p95)}` : undefined}
        />
        <Kpi
          icon={<Clock size={14} />}
          label="Duration / session"
          loading={sessions.initialLoading}
          value={ss ? duration(ss.avg_duration) : '—'}
          sub={ss ? `p50 ${duration(ss.duration_p50)} · p95 ${duration(ss.duration_p95)}` : undefined}
        />
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens / session"
          loading={sessions.initialLoading}
          value={ss ? compact(ss.avg_tokens) : '—'}
          sub={ss ? `p50 ${compact(ss.tokens_p50)} · p95 ${compact(ss.tokens_p95)}` : undefined}
        />
        <Kpi
          icon={<Wrench size={14} />}
          label="Tools / session"
          loading={sessions.initialLoading}
          value={ss ? ss.avg_tool_calls.toFixed(1) : '—'}
          sub={ss ? `p50 ${int(ss.tool_calls_p50)} · p95 ${int(ss.tool_calls_p95)}` : undefined}
        />
      </div>

      {/* Recent activity — ActivityFeed renders its own title row + sort tabs. */}
      <ActivityFeed since={since} until={until} />
    </Page>
  );
}

// ---- Building blocks ----

interface KpiProps {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub?: React.ReactNode;
  loading?: boolean;
  danger?: boolean;
}

function Kpi({ icon, label, value, sub, loading, danger }: KpiProps) {
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

function SectionTitle({ name, hint, info, control }: { name: string; hint?: string; info?: React.ReactNode; control?: React.ReactNode }) {
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

function Panel({
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

/** Renders skeleton / error / empty / chart for a useFetch-backed panel body. */
function Frame({
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

/** Convert a #rrggbb hex color to [hue, saturation, lightness] (h in 0..360, s/l in 0..1). */
function hexToHsl(hex: string): [number, number, number] {
  const m = hex.replace('#', '');
  const r = parseInt(m.slice(0, 2), 16) / 255;
  const g = parseInt(m.slice(2, 4), 16) / 255;
  const b = parseInt(m.slice(4, 6), 16) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (d === 0) return [0, 0, l];
  const s = d / (1 - Math.abs(2 * l - 1));
  let h: number;
  if (max === r) h = (((g - b) / d) % 6 + 6) % 6;
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  return [h * 60, s, l];
}

/**
 * Assigns a distinct color to every series key for the current Usage dimension.
 * Keys are grouped by their brand — the model creator's brand for the `model`
 * dimension, the provider's brand for `vendor` — then same-brand siblings are
 * spread across a wide lightness+hue ramp so they stay clearly distinguishable
 * in a stacked bar (e.g. Claude Opus vs Haiku, which used to collide). Anchoring
 * to the brand color keeps a series on-brand relative to other vendors. Keys we
 * can't brand (users, unknown vendors) get evenly-spaced categorical hues.
 * "Other" is always the muted grey.
 *
 * A key's exact shade depends on which same-brand siblings are currently shown,
 * not on its name alone — the deliberate cost of guaranteeing sibling contrast.
 */
function assignSeriesColors(keys: string[], dim: UsageDimension): Record<string, string> {
  const baseColor = (k: string): string | null => {
    if (dim === 'model') return brandOf(k)?.color ?? null;
    if (dim === 'vendor') return providerBrand(k, [])?.color ?? null;
    return null; // users have no brand
  };

  // Partition into brand groups (keyed by base hex) plus one unbranded bucket.
  const branded = new Map<string, string[]>();
  const unbranded: string[] = [];
  for (const k of keys) {
    if (k === 'Other') continue;
    const base = baseColor(k);
    if (base) {
      const g = branded.get(base);
      if (g) g.push(k);
      else branded.set(base, [k]);
    } else {
      unbranded.push(k);
    }
  }

  const out: Record<string, string> = {};
  for (const [base, group] of branded) {
    const [h, s] = hexToHsl(base);
    const sat = Math.round(Math.min(0.85, Math.max(0.5, s)) * 100);
    const sorted = [...group].sort();
    const n = sorted.length;
    sorted.forEach((k, i) => {
      const t = n === 1 ? 0.5 : i / (n - 1); // 0..1 position within the group
      const light = Math.round(40 + t * 32); // 40%..72%
      const hue = (((h + (t - 0.5) * 34) % 360) + 360) % 360; // ±17° spread
      out[k] = `hsl(${hue} ${sat}% ${light}%)`;
    });
  }

  // Unbranded keys: evenly-spaced hues starting near the pine-green accent.
  const sortedU = [...unbranded].sort();
  const nu = sortedU.length;
  sortedU.forEach((k, i) => {
    const hue = Math.round(150 + (i / Math.max(1, nu)) * 300) % 360;
    out[k] = `hsl(${hue} 55% 55%)`;
  });

  out.Other = 'var(--text-muted)';
  return out;
}

/** Compact large numbers for axis ticks, e.g. 12.3k, 4.5M. */
function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return `${Math.round(n)}`;
}

// ---- Success bar-table + error codes ----

// Success-rate → band color: green ≥ 99%, amber ≥ 90%, red below. null (no
// traffic) → muted grey. Bars and the overall % share this scale.
function bandColor(rate: number | null): string {
  if (rate == null) return 'var(--text-muted)';
  if (rate >= 0.99) return 'var(--chart-1)';
  if (rate >= 0.9) return 'var(--amber)';
  return 'var(--danger)';
}

interface SuccessBar {
  req: number;
  rate: number | null;
  label: string;
}

/**
 * A compact strip of vertical bars, one per time bucket (same buckets as the old
 * line chart). Height AND color both encode the bucket's success rate on a fixed
 * 0–100% scale — a short bar always means a bad bucket, regardless of the current
 * view or traffic. No-traffic buckets (rate=null) render a flat baseline tick so
 * bars stay time-aligned across rows; a genuinely-failing bucket keeps a small
 * floor so a 0%-ok bar stays a visible red sliver, distinct from the null tick.
 */
function BarStrip({ bars }: { bars: SuccessBar[] }) {
  return (
    <div className={styles.barStrip} aria-hidden="true">
      {bars.map((b, i) => {
        const h = b.rate == null ? 0 : Math.max(6, b.rate * 100);
        return (
          <div key={i} className={styles.barSlot}>
            <div
              className={styles.bar}
              style={{ height: `${h}%`, background: bandColor(b.rate) }}
              title={
                b.rate == null
                  ? `${b.label}: no requests`
                  : `${b.label}: ${Math.round(b.rate * 100)}% ok · ${b.req} req`
              }
            />
          </div>
        );
      })}
    </div>
  );
}

// Short human label for an upstream status. 0 = transport failure (no response);
// 429 rate-limited; other 4xx client; 5xx server.
function errorReason(status: number): string {
  if (status === 0) return 'Transport error';
  if (status === 429) return 'Rate limited';
  if (status >= 500) return 'Server error';
  if (status >= 400) return 'Client error';
  return `Status ${status}`;
}

// Pill/proportion-bar color for a status class.
function errorColor(status: number): string {
  if (status === 429) return 'var(--amber)';
  if (status === 0 || status >= 500) return 'var(--danger)';
  return 'var(--text-muted)'; // other 4xx
}

/** Ranked list of top error codes with a status pill, reason, count, and a
 *  proportion bar (relative to the largest count). */
function ErrorCodeList({ rows }: { rows: ErrorCodeRow[] }) {
  const max = rows.reduce((m, r) => Math.max(m, r.count), 0);
  return (
    <div className={styles.ecList} role="list">
      {rows.map((r) => {
        const color = errorColor(r.status);
        return (
          <div key={r.status} className={styles.ecRow} role="listitem">
            <span className={styles.ecStatus} style={{ color, borderColor: color }}>
              {r.status === 0 ? 'ERR' : r.status}
            </span>
            <span className={styles.ecReason}>{errorReason(r.status)}</span>
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
