import { useMemo, useState } from 'react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  XAxis,
  YAxis,
} from 'recharts';
import { Activity, Clock, Coins, DollarSign, GitBranch, MessageSquare, ShieldCheck, Users } from 'lucide-react';
import { api } from '../api/client';
import type { Bucket, UsageDimension } from '../api/types';
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
import { brandOf, providerBrand } from '../lib/modelBrand';
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
const TOP_N = 6;

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
  const tick = useLiveTick(REFRESH_MS);
  const range = RANGES.find((r) => r.key === rangeKey) ?? RANGES[0];

  const { since, until } = useMemo(() => {
    const u = tick + 1;
    return { since: u - range.seconds, until: u };
  }, [tick, range]);

  const opts = { intervalMs: REFRESH_MS };
  const overview = useFetch(() => api.overview(since, until), [since, until], opts);
  const sessions = useFetch(() => api.sessionsOverview(since, until), [since, until], opts);
  const series = useFetch(
    () => api.series(since, until, range.bucket),
    [since, until, range.bucket],
    opts,
  );
  const tokenSeries = useFetch(
    () => api.tokensByModel(since, until, range.bucket, usageDim),
    [since, until, range.bucket, usageDim],
    opts,
  );
  const byVendor = useFetch(() => api.breakdown('vendor', since, until), [since, until], opts);
  // Resolve user-id series keys to display names for the by-user Usage view.
  const usersList = useFetch(() => api.users(), [], opts);
  const composition = useFetch(() => api.contextComposition(since, until), [since, until], opts);
  const errs = useFetch(() => api.errors(since, until), [since, until], opts);

  const ov = overview.data;
  const ss = sessions.data;

  // Time-series rows with derived ratios for the charts.
  const points = useMemo(() => {
    const pts = series.data?.points ?? [];
    const bucket = series.data?.bucket ?? range.bucket;
    return pts.map((p) => ({
      label: bucketLabel(p.ts, bucket),
      requests: p.requests,
      errors: p.errors,
      cost: p.cost,
      input_tokens: p.input_tokens,
      output_tokens: p.output_tokens,
      cache_read_input_tokens: p.cache_read_input_tokens,
      cache_creation_input_tokens: p.cache_creation_input_tokens,
      avg_latency_ms: p.avg_latency_ms,
      avg_ttft_ms: p.avg_ttft_ms,
      avg_output_tokens_per_second: p.avg_output_tokens_per_second,
      success: p.requests > 0 ? ((p.requests - p.errors) / p.requests) * 100 : null,
      // Cache hit rate = cache reads / total input (fresh + cache read + cache write).
      cache_hit: (() => {
        const totalInput = p.input_tokens + p.cache_read_input_tokens + p.cache_creation_input_tokens;
        return totalInput > 0 ? (p.cache_read_input_tokens / totalInput) * 100 : null;
      })(),
    }));
  }, [series.data, range.bucket]);
  const seriesEmpty = points.length === 0;

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

  const vendors = (byVendor.data?.rows ?? []).slice(0, TOP_N);

  const errorClasses = useMemo(() => {
    const e = errs.data;
    if (!e) return [];
    return [
      { name: '429', value: e.rate_limited, fill: 'var(--chart-3)' },
      { name: '4xx', value: e.client_error, fill: 'var(--chart-5)' },
      { name: '5xx', value: e.server_error, fill: 'var(--danger)' },
      { name: 'transport', value: e.transport, fill: 'var(--chart-4)' },
    ];
  }, [errs.data]);
  const errorsEmpty = errorClasses.every((c) => c.value === 0);
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
      <SectionTitle name="Performance" hint="TTFT and output generation speed" />
      <div className={styles.grid2}>
        <Panel title="Avg TTFT">
          <Frame r={series} height={styles.chartXs} empty={seriesEmpty}>
            <ChartContainer config={{ avg_ttft_ms: { label: 'Avg TTFT', color: 'var(--chart-3)' } }} className={CHART_CLS}>
              <LineChart data={points} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => `${Math.round(v)}`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Line dataKey="avg_ttft_ms" type="monotone" stroke="var(--color-avg_ttft_ms)" strokeWidth={2} dot={false} />
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Avg throughput">
          <Frame r={series} height={styles.chartXs} empty={seriesEmpty}>
            <ChartContainer config={{ avg_output_tokens_per_second: { label: 'Output tok/s', color: 'var(--chart-2)' } }} className={CHART_CLS}>
              <LineChart data={points} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Line dataKey="avg_output_tokens_per_second" type="monotone" stroke="var(--color-avg_output_tokens_per_second)" strokeWidth={2} dot={false} />
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>

      {/* Reliability */}
      <SectionTitle name="Reliability" />
      <div className={styles.grid3}>
        <Panel title="Success rate over time">
          <Frame r={series} height={styles.chartSm} empty={seriesEmpty}>
            <ChartContainer config={{ success: { label: 'Success', color: 'var(--chart-1)' } }} className={CHART_CLS}>
              <LineChart data={points} margin={{ top: 6, right: 8, left: -16, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={40} domain={[0, 100]} tickFormatter={(v: number) => `${v}%`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Line dataKey="success" type="monotone" stroke="var(--color-success)" strokeWidth={2} dot={false} connectNulls />
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Errors by class">
          <Frame r={errs} height={styles.chartSm} empty={errorsEmpty}>
            <ChartContainer config={{}} className={CHART_CLS}>
              <BarChart data={errorClasses} layout="vertical" margin={{ top: 2, right: 12, left: 2, bottom: 2 }}>
                <XAxis type="number" hide allowDecimals={false} />
                <YAxis type="category" dataKey="name" width={72} tickLine={false} axisLine={false} tick={{ fontSize: 11 }} />
                <ChartTooltip content={<ChartTooltipContent nameKey="name" hideLabel />} />
                <Bar dataKey="value" radius={3}>
                  {errorClasses.map((c) => (
                    <Cell key={c.name} fill={c.fill} />
                  ))}
                </Bar>
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Error rate by vendor">
          <Frame r={byVendor} height={styles.chartSm} empty={vendors.length === 0}>
            <CategoryBars
              rows={vendors.map((v) => ({ ...v, err_rate: v.requests > 0 ? (v.errors / v.requests) * 100 : 0 }))}
              dataKey="err_rate"
              label="Error rate"
              color="var(--chart-5)"
              fmt={(v) => `${v.toFixed(1)}%`}
            />
          </Frame>
        </Panel>
      </div>

      {/* Sessions */}
      <SectionTitle name="Sessions" hint="Agent runs — outcome inferred from each session's last call" />
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
      </div>
      <div className={`card ${styles.panel}`}>
        <div className={styles.panelHead}>
          <span className={styles.panelTitle}>Outcomes</span>
        </div>
        <Frame r={sessions} height="" empty={!ss || ss.sessions === 0}>
          {ss ? (
            <OutcomeBar completed={ss.completed} interrupted={ss.interrupted} errored={ss.errored} />
          ) : null}
        </Frame>
      </div>

      {/* Recent activity */}
      <SectionTitle name="Recent activity" />
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

function Panel({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className={`card ${styles.panel}`}>
      <div className={styles.panelHead}>
        <span className={styles.panelTitle}>{title}</span>
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

/** Horizontal bar chart over breakdown rows (single metric), keyed by `.key`. */
function CategoryBars<T extends { key: string }>({
  rows,
  dataKey,
  label,
  color,
  fmt,
}: {
  rows: T[];
  dataKey: string;
  label: string;
  color: string;
  fmt: (n: number) => string;
}) {
  const config: ChartConfig = { [dataKey]: { label, color } };
  return (
    <ChartContainer config={config} className={CHART_CLS}>
      <BarChart data={rows} layout="vertical" margin={{ top: 2, right: 14, left: 2, bottom: 2 }}>
        <XAxis type="number" hide tickFormatter={fmt} />
        <YAxis type="category" dataKey="key" width={104} tickLine={false} axisLine={false} tick={{ fontSize: 11 }} />
        <ChartTooltip content={<ChartTooltipContent />} />
        <Bar dataKey={dataKey} fill={`var(--color-${dataKey})`} radius={3} />
      </BarChart>
    </ChartContainer>
  );
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

// Inferred session outcomes, in bar/legend order.
const OUTCOMES = [
  { key: 'completed', label: 'Completed', color: 'var(--chart-1)' },
  { key: 'interrupted', label: 'Interrupted', color: 'var(--chart-3)' },
  { key: 'errored', label: 'Errored', color: 'var(--danger)' },
] as const;

/** Stacked proportion bar + legend for a session's completed/interrupted/errored mix. */
function OutcomeBar({
  completed,
  interrupted,
  errored,
}: {
  completed: number;
  interrupted: number;
  errored: number;
}) {
  const counts = { completed, interrupted, errored };
  const total = completed + interrupted + errored;
  return (
    <div className={styles.outcome}>
      <div className={styles.outcomeBar}>
        {OUTCOMES.map((o) => {
          const v = counts[o.key];
          if (v === 0 || total === 0) return null;
          return (
            <div
              key={o.key}
              className={styles.outcomeSeg}
              style={{ width: `${(v / total) * 100}%`, background: o.color }}
              title={`${o.label}: ${v}`}
            />
          );
        })}
      </div>
      <div className={styles.outcomeLegend}>
        {OUTCOMES.map((o) => {
          const v = counts[o.key];
          const pct = total > 0 ? (v / total) * 100 : 0;
          return (
            <div key={o.key} className={styles.outcomeItem}>
              <span className={styles.outcomeDot} style={{ background: o.color }} />
              <span className={styles.outcomeLabel}>{o.label}</span>
              <span className={styles.outcomeCount}>{int(v)}</span>
              <span className={styles.outcomePct}>{pct.toFixed(0)}%</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
