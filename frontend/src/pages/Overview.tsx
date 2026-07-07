import { useMemo, useState } from 'react';
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  ComposedChart,
  Line,
  LineChart,
  Pie,
  PieChart,
  XAxis,
  YAxis,
} from 'recharts';
import { Activity, Clock, Coins, DollarSign, GitBranch, MessageSquare, ShieldCheck, Users } from 'lucide-react';
import { api } from '../api/client';
import type { Bucket, BreakdownRow } from '../api/types';
import { ContextSunburst } from '../components/ContextSunburst';
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
import { bucketLabel, duration, int, money, ms, percent } from '../lib/format';
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
const CHART_CLS = 'aspect-auto h-full w-full';
const PALETTE = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)'];

type FetchLike = { initialLoading: boolean; error: string | null; refetch: () => void };

export function OverviewPage() {
  const [rangeKey, setRangeKey] = useState('24h');
  const [bdDim, setBdDim] = useState<'model' | 'vendor' | 'user'>('model');
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
    () => api.tokensByModel(since, until, range.bucket),
    [since, until, range.bucket],
    opts,
  );
  const byModel = useFetch(() => api.breakdown('model', since, until), [since, until], opts);
  const byVendor = useFetch(() => api.breakdown('vendor', since, until), [since, until], opts);
  const byUser = useFetch(() => api.breakdown('user', since, until), [since, until], opts);
  const byModality = useFetch(() => api.breakdown('modality', since, until), [since, until], opts);
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
      avg_latency_ms: p.avg_latency_ms,
      success: p.requests > 0 ? ((p.requests - p.errors) / p.requests) * 100 : null,
      cache_hit: p.input_tokens > 0 ? (p.cached_tokens / p.input_tokens) * 100 : null,
    }));
  }, [series.data, range.bucket]);
  const seriesEmpty = points.length === 0;

  // Tokens-by-model combo chart: one row per bucket with a numeric column per
  // model plus total cost. Bars stack the model columns (left axis); cost is a
  // line (right axis). Models come pre-ranked from the backend (top N + "Other").
  const { tokenModels, tokenPoints } = useMemo(() => {
    const data = tokenSeries.data;
    const modelKeys = data?.models ?? [];
    const bucket = data?.bucket ?? range.bucket;
    const rows = (data?.points ?? []).map((p) => {
      const row: Record<string, number | string> = { label: bucketLabel(p.ts, bucket), cost: p.cost };
      for (const m of modelKeys) row[m] = p.tokens[m] ?? 0;
      return row;
    });
    return { tokenModels: modelKeys, tokenPoints: rows };
  }, [tokenSeries.data, range.bucket]);
  const tokenConfig = useMemo<ChartConfig>(() => {
    const c: ChartConfig = { cost: { label: 'Cost' } };
    tokenModels.forEach((m) => {
      c[m] = { label: m };
    });
    return c;
  }, [tokenModels]);
  const modelColor = (m: string, i: number) =>
    m === 'Other' ? 'var(--text-muted)' : PALETTE[i % PALETTE.length];

  const models = (byModel.data?.rows ?? []).slice(0, TOP_N);
  const vendors = (byVendor.data?.rows ?? []).slice(0, TOP_N);
  const modalities = byModality.data?.rows ?? [];
  const bdRows =
    bdDim === 'model'
      ? byModel.data?.rows
      : bdDim === 'vendor'
        ? byVendor.data?.rows
        : byUser.data?.rows;
  const bdResult = bdDim === 'model' ? byModel : bdDim === 'vendor' ? byVendor : byUser;

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
          label={`Requests (${range.label})`}
          loading={overview.initialLoading}
          value={ov ? int(ov.requests) : '—'}
          sub={ov ? `${int(ov.errors)} failed` : undefined}
        />
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens"
          loading={overview.initialLoading}
          value={ov ? int(ov.tokens.input + ov.tokens.output) : '—'}
          sub={ov ? `${int(ov.tokens.input)} in · ${int(ov.tokens.output)} out` : undefined}
        />
        <Kpi
          icon={<DollarSign size={14} />}
          label={`Spend (${range.label})`}
          loading={overview.initialLoading}
          value={ov ? money(ov.total_spend) : '—'}
          sub={ov ? `${money(ov.daily_burn)}/day` : undefined}
        />
        <Kpi
          icon={<Users size={14} />}
          label="Active users"
          loading={overview.initialLoading}
          value={ov ? int(ov.active_callers) : '—'}
          sub={ov ? `${int(ov.users_active)} provisioned` : undefined}
        />
        <Kpi
          icon={<ShieldCheck size={14} />}
          label="Success rate"
          loading={overview.initialLoading}
          value={ov ? percent(1 - ov.error_rate) : '—'}
          danger={ov != null && ov.error_rate > 0.05}
          sub={ov ? `${percent(ov.error_rate)} errors` : undefined}
        />
      </div>

      {/* Usage */}
      <SectionTitle name="Usage" hint="Requests and spend" />
      <div className={`card ${styles.panel}`} style={{ marginBottom: 12 }}>
        <div className={styles.panelHead}>
          <span className={styles.panelTitle}>Tokens by model & cost</span>
        </div>
        <Frame r={tokenSeries} height={styles.chartLg} empty={tokenModels.length === 0}>
          <ChartContainer config={tokenConfig} className={CHART_CLS}>
            <ComposedChart data={tokenPoints} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
              <CartesianGrid vertical={false} />
              <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
              <YAxis
                yAxisId="tokens"
                tickLine={false}
                axisLine={false}
                width={48}
                tickFormatter={(v: number) => compact(v)}
              />
              <YAxis
                yAxisId="cost"
                orientation="right"
                tickLine={false}
                axisLine={false}
                width={52}
                tickFormatter={(v: number) => money(v)}
              />
              <ChartTooltip content={<ChartTooltipContent />} />
              <ChartLegend content={<ChartLegendContent />} />
              {tokenModels.map((m, i) => (
                <Bar
                  key={m}
                  yAxisId="tokens"
                  dataKey={m}
                  stackId="tok"
                  fill={modelColor(m, i)}
                  radius={i === tokenModels.length - 1 ? [3, 3, 0, 0] : undefined}
                />
              ))}
              <Line
                yAxisId="cost"
                dataKey="cost"
                type="monotone"
                stroke="var(--text)"
                strokeWidth={2}
                dot={false}
              />
            </ComposedChart>
          </ChartContainer>
        </Frame>
      </div>
      <div className={styles.grid2}>
        <Panel title="Requests over time">
          <Frame r={series} height={styles.chartSm} empty={seriesEmpty}>
            <ChartContainer config={{ requests: { label: 'Requests', color: 'var(--chart-1)' } }} className={CHART_CLS}>
              <AreaChart data={points} margin={{ top: 6, right: 8, left: -12, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={40} allowDecimals={false} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Area dataKey="requests" type="monotone" stroke="var(--color-requests)" fill="var(--color-requests)" fillOpacity={0.15} strokeWidth={2} />
              </AreaChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Spend over time">
          <Frame r={series} height={styles.chartSm} empty={seriesEmpty}>
            <ChartContainer config={{ cost: { label: 'Spend', color: 'var(--chart-1)' } }} className={CHART_CLS}>
              <AreaChart data={points} margin={{ top: 6, right: 8, left: -4, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={52} tickFormatter={(v: number) => money(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Area dataKey="cost" type="monotone" stroke="var(--color-cost)" fill="var(--color-cost)" fillOpacity={0.15} strokeWidth={2} />
              </AreaChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>
      <div className={styles.grid3}>
        <Panel title="Top models">
          <Frame r={byModel} height={styles.chartSm} empty={models.length === 0}>
            <CategoryBars rows={models} dataKey="requests" label="Requests" color="var(--chart-1)" fmt={int} />
          </Frame>
        </Panel>
        <Panel title="By vendor">
          <Frame r={byVendor} height={styles.chartSm} empty={vendors.length === 0}>
            <ChartContainer config={{}} className={CHART_CLS}>
              <PieChart>
                <ChartTooltip content={<ChartTooltipContent nameKey="key" />} />
                <Pie data={vendors} dataKey="requests" nameKey="key" innerRadius={42} outerRadius={68} strokeWidth={2} stroke="var(--surface)">
                  {vendors.map((_, i) => (
                    <Cell key={i} fill={PALETTE[i % PALETTE.length]} />
                  ))}
                </Pie>
              </PieChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Spend by model">
          <Frame r={byModel} height={styles.chartSm} empty={models.length === 0}>
            <CategoryBars rows={models} dataKey="cost" label="Spend" color="var(--chart-1)" fmt={money} />
          </Frame>
        </Panel>
      </div>

      {/* Performance */}
      <SectionTitle name="Performance" hint="End-to-end latency" />
      <div className={styles.grid2}>
        <Panel title="Latency percentiles">
          {overview.initialLoading ? (
            <Skeleton height={64} />
          ) : (
            <div className={styles.latencyGroup} style={{ marginTop: 4 }}>
              {(['p50', 'p95', 'p99'] as const).map((p) => (
                <div key={p} className={styles.latencyStat}>
                  <span className={styles.latencyLabel}>{p}</span>
                  <span className={styles.latencyValue}>{ov ? ms(ov.latency_ms[p]) : '—'}</span>
                </div>
              ))}
            </div>
          )}
          <div className={styles.deferred}>TTFT &amp; tokens/sec coming once streaming is instrumented.</div>
        </Panel>
        <Panel title="Avg latency over time">
          <Frame r={series} height={styles.chartXs} empty={seriesEmpty}>
            <ChartContainer config={{ avg_latency_ms: { label: 'Avg latency', color: 'var(--chart-4)' } }} className={CHART_CLS}>
              <LineChart data={points} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => `${Math.round(v)}`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Line dataKey="avg_latency_ms" type="monotone" stroke="var(--color-avg_latency_ms)" strokeWidth={2} dot={false} />
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>

      {/* Tokens */}
      <SectionTitle name="Tokens" hint="LLM usage — input, output, and cache hits" />
      <div className={styles.grid3}>
        <Panel title="Token throughput">
          <Frame r={series} height={styles.chartSm} empty={seriesEmpty}>
            <ChartContainer
              config={{
                input_tokens: { label: 'Input', color: 'var(--chart-1)' },
                output_tokens: { label: 'Output', color: 'var(--chart-2)' },
              }}
              className={CHART_CLS}
            >
              <AreaChart data={points} margin={{ top: 6, right: 8, left: -4, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Area dataKey="input_tokens" stackId="t" type="monotone" stroke="var(--color-input_tokens)" fill="var(--color-input_tokens)" fillOpacity={0.18} strokeWidth={2} />
                <Area dataKey="output_tokens" stackId="t" type="monotone" stroke="var(--color-output_tokens)" fill="var(--color-output_tokens)" fillOpacity={0.18} strokeWidth={2} />
              </AreaChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Cache-hit ratio">
          <Frame r={series} height={styles.chartSm} empty={seriesEmpty}>
            <ChartContainer config={{ cache_hit: { label: 'Cache hit', color: 'var(--chart-3)' } }} className={CHART_CLS}>
              <LineChart data={points} margin={{ top: 6, right: 8, left: -16, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={40} domain={[0, 100]} tickFormatter={(v: number) => `${v}%`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Line dataKey="cache_hit" type="monotone" stroke="var(--color-cache_hit)" strokeWidth={2} dot={false} connectNulls />
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Tokens by model">
          <Frame r={byModel} height={styles.chartSm} empty={models.length === 0}>
            <ChartContainer
              config={{
                input_tokens: { label: 'Input', color: 'var(--chart-1)' },
                output_tokens: { label: 'Output', color: 'var(--chart-2)' },
              }}
              className={CHART_CLS}
            >
              <BarChart data={models} layout="vertical" margin={{ top: 2, right: 12, left: 2, bottom: 2 }}>
                <XAxis type="number" hide tickFormatter={(v: number) => compact(v)} />
                <YAxis type="category" dataKey="key" width={104} tickLine={false} axisLine={false} tick={{ fontSize: 11 }} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <Bar dataKey="input_tokens" stackId="t" fill="var(--color-input_tokens)" radius={[3, 0, 0, 3]} />
                <Bar dataKey="output_tokens" stackId="t" fill="var(--color-output_tokens)" radius={[0, 3, 3, 0]} />
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>

      {/* Context distribution */}
      <SectionTitle name="Context distribution" hint="Where the average context window goes" />
      <div className={`card ${styles.panel}`}>
        <Frame r={composition} height="" empty={(composition.data?.sources.length ?? 0) === 0}>
          {composition.data ? <ContextSunburst data={composition.data} /> : null}
        </Frame>
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

      {/* Modality mix — the multi-task view */}
      <SectionTitle name="Modality mix" hint="All tasks served by the proxy" />
      <div className={`card ${styles.panel}`}>
        <Frame r={byModality} height="" empty={modalities.length === 0}>
          <BreakdownTable rows={modalities} keyLabel="Modality" capitalize />
        </Frame>
      </div>

      {/* Breakdown table */}
      <SectionTitle
        name="Breakdown"
        control={
          <div className={styles.seg} role="tablist" aria-label="Breakdown dimension">
            {(['model', 'vendor', 'user'] as const).map((d) => (
              <button
                key={d}
                role="tab"
                aria-selected={d === bdDim}
                className={`${styles.segBtn} ${d === bdDim ? styles.segActive : ''}`}
                onClick={() => setBdDim(d)}
              >
                {d[0].toUpperCase() + d.slice(1)}
              </button>
            ))}
          </div>
        }
      />
      <div className={`card ${styles.panel}`}>
        <Frame r={bdResult} height="" empty={(bdRows?.length ?? 0) === 0}>
          <BreakdownTable rows={bdRows ?? []} keyLabel={bdDim[0].toUpperCase() + bdDim.slice(1)} />
        </Frame>
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
  sub?: string;
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

function SectionTitle({ name, hint, control }: { name: string; hint?: string; control?: React.ReactNode }) {
  return (
    <div className={styles.sectionTitle}>
      <span className={styles.sectionName}>{name}</span>
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

/** Tabular breakdown: requests, success %, avg latency, tokens, spend. */
function BreakdownTable({
  rows,
  keyLabel,
  capitalize,
}: {
  rows: BreakdownRow[];
  keyLabel: string;
  capitalize?: boolean;
}) {
  return (
    <div className={styles.tableScroll}>
      <table className="table">
        <thead>
          <tr>
            <th>{keyLabel}</th>
            <th className="num">Requests</th>
            <th className="num">Success</th>
            <th className="num">Avg latency</th>
            <th className="num">Tokens</th>
            <th className="num">Spend</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const success = r.requests > 0 ? (r.requests - r.errors) / r.requests : 1;
            return (
              <tr key={r.key}>
                <td style={capitalize ? { textTransform: 'capitalize' } : undefined}>{r.key || '—'}</td>
                <td className="num">{int(r.requests)}</td>
                <td className="num">{percent(success)}</td>
                <td className="num">{ms(r.avg_latency_ms)}</td>
                <td className="num">{int(r.input_tokens + r.output_tokens)}</td>
                <td className="num">{money(r.cost)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
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
