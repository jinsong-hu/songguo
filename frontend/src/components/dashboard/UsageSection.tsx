import { useMemo, useState } from 'react';
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts';
import { api } from '../../api/client';
import type { UsageDimension } from '../../api/types';
import { useFetch } from '../../lib/useFetch';
import { bucketLabel, money } from '../../lib/format';
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
 * Tokens and cost over time, stacked by the selected breakdown. Both charts read
 * one response — the series endpoint carries tokens and costs per key in the
 * same points — so they can never disagree about which keys are in view.
 */
export function UsageSection({
  scope,
  defaultDim = 'model',
  title = 'Usage',
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

  // One row per bucket with a numeric column per key. Bars stack the columns.
  // Keys come pre-ranked from the backend (top N + "Other").
  const { keys, tokenPoints, costPoints } = useMemo(() => {
    const data = series.data;
    const modelKeys = data?.models ?? [];
    const bucket = data?.bucket ?? scope.bucket;
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
    return { keys: modelKeys, tokenPoints: tokRows, costPoints: costRows };
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
            label="Usage breakdown dimension"
            options={scope.dims}
            value={dim}
            onChange={setDim}
          />
        }
      />
      <div className={styles.grid2}>
        <Panel title="Tokens">
          <Frame r={series} height={styles.chartSm} empty={empty}>
            <ChartContainer config={config} className={CHART_CLS}>
              <BarChart data={tokenPoints} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={28} />
                <YAxis tickLine={false} axisLine={false} width={48} tickFormatter={(v: number) => compact(v)} />
                <ChartTooltip content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                {keys.map((m, i) => (
                  <Bar
                    key={m}
                    dataKey={m}
                    stackId="tok"
                    fill={colors[m]}
                    radius={i === keys.length - 1 ? [3, 3, 0, 0] : undefined}
                  />
                ))}
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
        <Panel title="Cost">
          <Frame r={series} height={styles.chartSm} empty={empty}>
            <ChartContainer config={config} className={CHART_CLS}>
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
                          <span className={styles.costTipName}>{label(String(name))}</span>
                          <span className={styles.costTipVal}>{money(Number(value))}</span>
                        </div>
                      )}
                    />
                  }
                />
                <ChartLegend content={<ChartLegendContent />} />
                {keys.map((m, i) => (
                  <Bar
                    key={m}
                    dataKey={m}
                    stackId="cost"
                    fill={colors[m]}
                    radius={i === keys.length - 1 ? [3, 3, 0, 0] : undefined}
                  />
                ))}
              </BarChart>
            </ChartContainer>
          </Frame>
        </Panel>
      </div>
    </>
  );
}
