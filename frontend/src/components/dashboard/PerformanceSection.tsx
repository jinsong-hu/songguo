import { useMemo, useState } from 'react';
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from 'recharts';
import { api } from '../../api/client';
import type { UsageDimension } from '../../api/types';
import { useFetch } from '../../lib/useFetch';
import { bucketLabel } from '../../lib/format';
import { assignSeriesColors, compact } from '../../lib/seriesColors';
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
 * Time-to-first-token and output throughput over time, on OpenRouter's
 * definition: TTFT is the wait for the first token, throughput counts only the
 * generation that follows it, and both are reported as the median (p50) over
 * the bucket's calls — never a mean, which one buffering relay's flush can move
 * by orders of magnitude. Reuses the usage series endpoint, which carries
 * per-key ttft/tps maps alongside the token sums, with its own independent
 * breakdown selector.
 *
 * Those two maps are sparse. A key with no streamed call in a bucket is absent,
 * and stays absent here as `null` — `connectNulls` bridges the gap, so an idle
 * hour joins the measurements on either side instead of diving to 0.
 */
export function PerformanceSection({
  scope,
  defaultDim = 'model',
  title = 'Performance',
}: {
  scope: SeriesScope;
  defaultDim?: UsageDimension;
  title?: string;
}) {
  const [dim, setDim] = useState<UsageDimension>(() => initialDim(scope, defaultDim));

  const series = useFetch(
    () => api.tokensByModel(scope.since, scope.until, scope.bucket, dim, scope.filter),
    [scope.since, scope.until, scope.bucket, dim, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  const { keys, ttftPoints, tpsPoints } = useMemo(() => {
    const data = series.data;
    const ks = data?.models ?? [];
    const bucket = data?.bucket ?? scope.bucket;
    const ttftRows: Record<string, number | string | null>[] = [];
    const tpsRows: Record<string, number | string | null>[] = [];
    for (const p of data?.points ?? []) {
      const label = bucketLabel(p.ts, bucket);
      const ttft: Record<string, number | string | null> = { label };
      const tps: Record<string, number | string | null> = { label };
      for (const k of ks) {
        ttft[k] = p.ttft[k] ?? null;
        tps[k] = p.tps[k] ?? null;
      }
      ttftRows.push(ttft);
      tpsRows.push(tps);
    }
    return { keys: ks, ttftPoints: ttftRows, tpsPoints: tpsRows };
  }, [series.data, scope.bucket]);

  const label = useMemo(() => seriesLabeller(scope, dim), [scope, dim]);
  const config = useMemo<ChartConfig>(() => {
    const c: ChartConfig = {};
    keys.forEach((k) => {
      c[k] = { label: label(k) };
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
            label="Performance breakdown dimension"
            options={scope.dims}
            value={dim}
            onChange={setDim}
          />
        }
      />
      <div className={styles.grid2}>
        <Panel title="TTFT (p50)">
          <Frame r={series} height={styles.chartXs} empty={empty}>
            <ChartContainer config={config} className={CHART_CLS}>
              <LineChart data={ttftPoints} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => `${Math.round(v)}`} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {keys.map((k) => (
                  <Line key={k} dataKey={k} type="monotone" stroke={colors[k]} strokeWidth={2} dot={false} connectNulls />
                ))}
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Throughput (p50)">
          <Frame r={series} height={styles.chartXs} empty={empty}>
            <ChartContainer config={config} className={CHART_CLS}>
              <LineChart data={tpsPoints} margin={{ top: 6, right: 8, left: -8, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {keys.map((k) => (
                  <Line key={k} dataKey={k} type="monotone" stroke={colors[k]} strokeWidth={2} dot={false} connectNulls />
                ))}
              </LineChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>
    </>
  );
}
