import { useMemo, useState } from 'react';
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
  type TooltipProps,
} from 'recharts';
import { AlertTriangle, DollarSign, Gauge, Server, Timer } from 'lucide-react';
import { api } from '../api/client';
import type { Bucket } from '../api/types';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { LIVE_REFRESH_MS, useFetch, useLiveTick } from '../lib/useFetch';
import { bucketLabel, int, money, ms, percent } from '../lib/format';
import { CallsTable } from './CallsTable';
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

export function OverviewPage() {
  const [rangeKey, setRangeKey] = useState('24h');
  // `tick` advances on the live refresh cadence so the window tracks "now".
  const tick = useLiveTick(REFRESH_MS);
  const range = RANGES.find((r) => r.key === rangeKey) ?? RANGES[0];

  const { since, until } = useMemo(() => {
    // `until` gets a +1s buffer so a call made in the current second isn't
    // excluded by the backend's half-open [since, until) window.
    const u = tick + 1;
    return { since: u - range.seconds, until: u };
  }, [tick, range]);

  const overview = useFetch(() => api.overview(since, until), [since, until], {
    intervalMs: REFRESH_MS,
  });
  const series = useFetch(
    () => api.series(since, until, range.bucket),
    [since, until, range.bucket],
    { intervalMs: REFRESH_MS },
  );

  const ov = overview.data;
  const runwayAmber = ov?.runway_days != null && ov.runway_days < 14;
  const errorElevated = ov != null && ov.error_rate > 0.05;

  const modalityEntries = useMemo(() => {
    if (!ov) return [];
    const entries = Object.entries(ov.spend_by_modality);
    entries.sort((a, b) => b[1] - a[1]);
    return entries;
  }, [ov]);
  const modalityMax = modalityEntries.reduce((m, [, v]) => Math.max(m, v), 0);

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
          icon={<DollarSign size={14} />}
          label={`Total spend (${range.label})`}
          loading={overview.initialLoading}
          value={ov ? money(ov.total_spend) : '—'}
        />
        <Kpi
          icon={<Gauge size={14} />}
          label="Budget runway"
          loading={overview.initialLoading}
          amber={runwayAmber}
          value={
            ov?.runway_days != null ? `≈ ${Math.round(ov.runway_days)} days` : '—'
          }
          sub={ov ? `${money(ov.daily_burn)} / day` : undefined}
        />
        <Kpi
          icon={<Server size={14} />}
          label="Active vendors"
          loading={overview.initialLoading}
          value={ov ? int(ov.vendors_active) : '—'}
          sub={ov ? `${int(ov.tokens_active)} tokens` : undefined}
        />
        <Kpi
          icon={<AlertTriangle size={14} />}
          label="Requests"
          loading={overview.initialLoading}
          value={ov ? int(ov.requests) : '—'}
          danger={errorElevated}
          sub={ov ? `${percent(ov.error_rate)} errors` : undefined}
        />
        <Kpi
          icon={<Timer size={14} />}
          label="p95 latency"
          loading={overview.initialLoading}
          value={ov ? ms(ov.latency_ms.p95) : '—'}
        />
      </div>

      {/* Chart + modality */}
      <div className={styles.row2}>
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelHead}>
            <span className={styles.panelTitle}>Spend over time</span>
          </div>
          <div className={styles.chartBox}>
            {series.initialLoading ? (
              <Skeleton height="100%" radius={6} />
            ) : series.error ? (
              <ErrorBanner message={series.error} onRetry={series.refetch} />
            ) : (
              <SpendChart
                points={series.data?.points ?? []}
                bucket={series.data?.bucket ?? range.bucket}
              />
            )}
          </div>
        </div>

        <div className={`card ${styles.panel}`}>
          <div className={styles.panelHead}>
            <span className={styles.panelTitle}>Spend by modality</span>
          </div>
          {overview.initialLoading ? (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} height={28} />
              ))}
            </div>
          ) : modalityEntries.length === 0 ? (
            <div className="muted" style={{ fontSize: 13 }}>
              No spend in this range.
            </div>
          ) : (
            <div className={styles.modalityList}>
              {modalityEntries.map(([name, cost]) => (
                <div key={name} className={styles.modalityRow}>
                  <div className={styles.modalityTop}>
                    <span className={styles.modalityName}>{name}</span>
                    <span className={styles.modalityCost}>{money(cost)}</span>
                  </div>
                  <div className={styles.bar}>
                    <div
                      className={styles.barFill}
                      style={{
                        width: modalityMax > 0 ? `${(cost / modalityMax) * 100}%` : '0%',
                      }}
                    />
                  </div>
                </div>
              ))}
            </div>
          )}

          <div
            style={{
              marginTop: 18,
              paddingTop: 16,
              borderTop: '1px solid var(--border)',
            }}
          >
            <span className={styles.panelTitle}>Latency</span>
            <div className={styles.latencyGroup} style={{ marginTop: 12 }}>
              {(['p50', 'p95', 'p99'] as const).map((p) => (
                <div key={p} className={styles.latencyStat}>
                  <span className={styles.latencyLabel}>{p}</span>
                  <span className={styles.latencyValue}>
                    {ov ? ms(ov.latency_ms[p]) : '—'}
                  </span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>

      {/* Recent calls */}
      <CallsTable since={since} until={until} />
    </Page>
  );
}

interface KpiProps {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub?: string;
  loading?: boolean;
  amber?: boolean;
  danger?: boolean;
}

function Kpi({ icon, label, value, sub, loading, amber, danger }: KpiProps) {
  return (
    <div
      className={`card ${styles.kpi} ${amber ? styles.kpiAmber : ''} ${
        danger ? styles.kpiDanger : ''
      }`}
    >
      <div className={styles.kpiLabel}>
        {icon}
        {label}
      </div>
      {loading ? (
        <Skeleton width={90} height={26} />
      ) : (
        <div className={styles.kpiValue}>{value}</div>
      )}
      {sub && !loading ? <div className={styles.kpiSub}>{sub}</div> : null}
    </div>
  );
}

function SpendChart({
  points,
  bucket,
}: {
  points: { ts: string; cost: number; requests: number }[];
  bucket: Bucket;
}) {
  const data = points.map((p) => ({
    label: bucketLabel(p.ts, bucket),
    cost: p.cost,
    requests: p.requests,
  }));

  return (
    <ResponsiveContainer width="100%" height="100%">
      <AreaChart data={data} margin={{ top: 6, right: 8, bottom: 0, left: -8 }}>
        <defs>
          <linearGradient id="spendFill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.22} />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity={0.02} />
          </linearGradient>
        </defs>
        <CartesianGrid stroke="var(--border)" vertical={false} />
        <XAxis
          dataKey="label"
          tick={{ fill: 'var(--text-muted)', fontSize: 11 }}
          tickLine={false}
          axisLine={{ stroke: 'var(--border)' }}
          minTickGap={28}
        />
        <YAxis
          tick={{ fill: 'var(--text-muted)', fontSize: 11 }}
          tickLine={false}
          axisLine={false}
          width={56}
          tickFormatter={(v: number) => money(v)}
        />
        <Tooltip content={<SpendTooltip />} cursor={{ stroke: 'var(--border)' }} />
        <Area
          type="monotone"
          dataKey="cost"
          stroke="var(--accent)"
          strokeWidth={2}
          fill="url(#spendFill)"
          dot={false}
          activeDot={{ r: 3, fill: 'var(--accent)' }}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}

function SpendTooltip({ active, payload, label }: TooltipProps<number, string>) {
  if (!active || !payload || payload.length === 0) return null;
  const point = payload[0].payload as { cost: number; requests: number };
  return (
    <div className={styles.tooltip}>
      <div className={styles.tooltipLabel}>{label}</div>
      <div className={styles.tooltipRow}>{money(point.cost)}</div>
      <div className={styles.tooltipRow} style={{ color: 'var(--text-muted)' }}>
        {int(point.requests)} requests
      </div>
    </div>
  );
}
