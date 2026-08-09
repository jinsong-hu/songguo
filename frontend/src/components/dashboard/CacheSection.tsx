import { useMemo, useState } from 'react';
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from 'recharts';
import { api } from '../../api/client';
import type { UsageDimension } from '../../api/types';
import { useFetch } from '../../lib/useFetch';
import { bucketLabel } from '../../lib/format';
import { assignSeriesColors } from '../../lib/seriesColors';
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '../ui/chart';
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

const CHART_CLS = 'aspect-auto h-full w-full';

/**
 * Cache-hit rate over time: cache reads as a share of total input tokens. Null
 * when a key had no input in a bucket, so the line breaks rather than dropping
 * to a 0% that never happened. Keys come pre-ranked by total input from the
 * backend (top N + "Other").
 */
export function CacheSection({
  scope,
  defaultDim = 'model',
  title = 'Cache',
}: {
  scope: SeriesScope;
  defaultDim?: UsageDimension;
  title?: string;
}) {
  const [dim, setDim] = useState<UsageDimension>(() => initialDim(scope, defaultDim));

  const series = useFetch(
    () => api.cacheByModel(scope.since, scope.until, scope.bucket, dim, scope.filter),
    [scope.since, scope.until, scope.bucket, dim, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  const { keys, points } = useMemo(() => {
    const data = series.data;
    const ks = data?.models ?? [];
    const bucket = data?.bucket ?? scope.bucket;
    const rows: Record<string, number | string | null>[] = [];
    for (const p of data?.points ?? []) {
      const row: Record<string, number | string | null> = { label: bucketLabel(p.ts, bucket) };
      for (const k of ks) {
        const input = p.input[k] ?? 0;
        const cacheRead = p.cache_read[k] ?? 0;
        row[k] = input > 0 ? (cacheRead / input) * 100 : null;
      }
      rows.push(row);
    }
    return { keys: ks, points: rows };
  }, [series.data, scope.bucket]);

  const label = useMemo(() => seriesLabeller(scope, dim), [scope, dim]);
  const config = useMemo<ChartConfig>(() => {
    const c: ChartConfig = {};
    keys.forEach((m) => {
      c[m] = { label: label(m) };
    });
    return c;
  }, [keys, label]);
  const colors = useMemo(() => assignSeriesColors(keys, dim), [keys, dim]);
  const empty = keys.length === 0;

  return (
    <>
      <SectionTitle
        name={title}
        control={
          <DimensionTabs
            label="Cache breakdown dimension"
            options={scope.dims}
            value={dim}
            onChange={setDim}
          />
        }
      />
      <Panel title="Cache hit rate">
        <Frame r={series} height={styles.chartSm} empty={empty}>
          <ChartContainer config={config} className={CHART_CLS}>
            {/* left margin 0, not the -16 this chart used to carry: the y axis
                reserves 40px for its labels, and pulling the plot 16px left
                clipped the widest one so "100%" rendered as "0%" — an axis
                reading 0/5/0/5/0 top to bottom. */}
            <LineChart data={points} margin={{ top: 6, right: 8, left: 0, bottom: 0 }}>
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
                        <span className={styles.costTipName}>{label(String(name))}</span>
                        <span className={styles.costTipVal}>
                          {value == null ? '—' : `${Number(value).toFixed(1)}%`}
                        </span>
                      </div>
                    )}
                  />
                }
              />
              <ChartLegend content={<ChartLegendContent />} />
              {keys.map((m) => (
                <Line
                  key={m}
                  dataKey={m}
                  type="monotone"
                  stroke={colors[m]}
                  strokeWidth={2}
                  dot={false}
                  connectNulls
                />
              ))}
            </LineChart>
          </ChartContainer>
        </Frame>
      </Panel>
    </>
  );
}
