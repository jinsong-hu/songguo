import { useMemo } from 'react';
import { Boxes, Coins, DatabaseZap, DollarSign, Sparkles } from 'lucide-react';
import { api } from '../api/client';
import { ContextSunburst, ContextDistributionCard } from '../components/ContextSunburst';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { CacheSection } from '../components/dashboard/CacheSection';
import { PerformanceSection } from '../components/dashboard/PerformanceSection';
import { SuccessSection } from '../components/dashboard/SuccessSection';
import { UsageSection } from '../components/dashboard/UsageSection';
import { Frame, Kpi, dashboardStyles as styles } from '../components/dashboard/Section';
import { useDashboard } from '../components/dashboard/useDashboard';
import { useFetch } from '../lib/useFetch';
import { int, money, percent } from '../lib/format';
import { compact } from '../lib/seriesColors';

/**
 * The gateway seen from the model side: where the budget goes, and what each
 * model's traffic is actually made of.
 *
 * This is where the context sunburst belongs more than anywhere else — the
 * composition of a window (system, tool schemas, history, tool results) is a
 * property of the model and the prompt, not of the provider relaying it — so it
 * sits near the top rather than buried mid-page.
 */
export function OverviewModelsPage() {
  const { scope, toolbar, overview, sessions, facets } = useDashboard({
    dimensions: ['model', 'vendor', 'client', 'user'],
    isUser: false,
  });

  const composition = useFetch(
    () => api.contextComposition(scope.since, scope.until, scope.filter),
    [scope.since, scope.until, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  const ov = overview.data;
  const ss = sessions.data;

  const cacheHit = useMemo(() => {
    if (!ov) return null;
    const totalInput = ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation;
    return totalInput > 0 ? ov.tokens.cached / totalInput : null;
  }, [ov]);

  // Cost per session, not per token: a model is cheap or expensive by what a
  // whole agent run on it costs, which is where the cache and the output/input
  // mix actually show up. Null when nothing ran, rather than a divide-by-zero 0.
  const costPerSession = ov && ss && ss.sessions > 0 ? ov.total_spend / ss.sessions : null;

  return (
    <Page title="Models" actions={toolbar}>
      {overview.error && (
        <div style={{ marginBottom: 16 }}>
          <ErrorBanner message={overview.error} onRetry={overview.refetch} />
        </div>
      )}

      <div className={styles.kpiGrid}>
        <Kpi
          icon={<Boxes size={14} />}
          label="Models with traffic"
          loading={facets.initialLoading}
          value={facets.data ? int(facets.data.models.length) : '—'}
        />
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens"
          loading={overview.initialLoading}
          value={
            ov
              ? int(ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation + ov.tokens.output)
              : '—'
          }
          sub={
            ov ? (
              <span className={styles.kpiSubSplit}>
                <span>{int(ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation)} in</span>
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
          icon={<DollarSign size={14} />}
          label="Cost / session"
          loading={overview.initialLoading || sessions.initialLoading}
          value={costPerSession == null ? '—' : money(costPerSession)}
          sub={ss ? `over ${int(ss.sessions)} sessions` : undefined}
        />
        <Kpi
          icon={<Sparkles size={14} />}
          label="Thinking tokens"
          loading={overview.initialLoading}
          value={ov ? compact(ov.tokens.thinking) : '—'}
          sub={
            ov && ov.tokens.output > 0
              ? `${percent(ov.tokens.thinking / ov.tokens.output)} of output`
              : undefined
          }
        />
        <Kpi
          icon={<DatabaseZap size={14} />}
          label="Cache hit"
          loading={overview.initialLoading}
          value={cacheHit == null ? '—' : percent(cacheHit)}
        />
      </div>

      <UsageSection scope={scope} defaultDim="model" title="Tokens & cost" />

      <ContextDistributionCard>
        <Frame r={composition} height="" empty={(composition.data?.sources.length ?? 0) === 0}>
          {composition.data ? (
            <ContextSunburst data={composition.data} centerLabel="across windows" />
          ) : null}
        </Frame>
      </ContextDistributionCard>

      <CacheSection scope={scope} defaultDim="model" />
      <PerformanceSection scope={scope} defaultDim="model" />
      <SuccessSection scope={scope} defaultDim="model" />
    </Page>
  );
}
