import { useMemo } from 'react';
import { Activity, Coins, DatabaseZap, DollarSign, ShieldCheck, Users } from 'lucide-react';
import { api } from '../api/client';
import { ContextSunburst, ContextDistributionCard } from '../components/ContextSunburst';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { BehavioralSection } from '../components/dashboard/BehavioralSection';
import { CacheSection } from '../components/dashboard/CacheSection';
import { PerformanceSection } from '../components/dashboard/PerformanceSection';
import { SuccessSection } from '../components/dashboard/SuccessSection';
import { UsageSection } from '../components/dashboard/UsageSection';
import { Frame, Kpi, dashboardStyles as styles } from '../components/dashboard/Section';
import { useDashboard } from '../components/dashboard/useDashboard';
import { useFetch } from '../lib/useFetch';
import { int, money, percent } from '../lib/format';
import { useSession } from '../lib/sessionContext';
import { ActivityFeed } from './ActivityFeed';

/**
 * The general dashboard: "how is the gateway doing right now", across
 * everything. Every section defaults to the by-model breakdown and can be
 * switched to any other — the per-purpose Overview pages (providers, models,
 * users, clients) are these same
 * sections with different defaults and a different selection.
 */
export function OverviewPage() {
  // A consumer key gets the same dashboard scoped by the backend to its own
  // traffic; the remaining fleet-only surfaces (active-users KPI, the by-user
  // breakdown, and the admin-only users roster) are hidden.
  const { me } = useSession();
  const isUser = me.role === 'user';

  const { scope, toolbar, overview, sessions, filtered, windowLabel } = useDashboard({
    dimensions: ['model', 'vendor', 'user'],
    isUser,
  });

  const composition = useFetch(
    () => api.contextComposition(scope.since, scope.until, scope.filter),
    [scope.since, scope.until, scope.filterKey],
    { intervalMs: scope.intervalMs },
  );

  const ov = overview.data;
  const ss = sessions.data;

  // Overall cache-hit ratio: cache reads / total input (fresh + cache read +
  // cache write). Null when there was no input in range, so the KPI shows "—"
  // rather than a misleading 0%.
  const cacheHit = useMemo(() => {
    if (!ov) return null;
    const totalInput = ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation;
    return totalInput > 0 ? ov.tokens.cached / totalInput : null;
  }, [ov]);

  return (
    <Page title="Overview" actions={toolbar}>
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
        {!isUser && (
          <Kpi
            icon={<Users size={14} />}
            label="Active users"
            loading={overview.initialLoading}
            value={ov ? int(ov.active_callers) : '—'}
          />
        )}
        <Kpi
          icon={<ShieldCheck size={14} />}
          label="Success rate"
          loading={overview.initialLoading}
          // error_rate divides by `rated`, so with nothing graded it is 0 and
          // would render a confident 100% over no evidence at all. Show "—"
          // instead, and name the refusals whenever there were any.
          value={ov ? (ov.rated > 0 ? percent(1 - ov.error_rate) : '—') : '—'}
          sub={ov && ov.denied > 0 ? `${int(ov.denied)} refused` : undefined}
          danger={ov != null && ov.rated > 0 && ov.error_rate > 0.05}
        />
        <Kpi
          icon={<DatabaseZap size={14} />}
          label="Cache hit"
          loading={overview.initialLoading}
          value={cacheHit == null ? '—' : percent(cacheHit)}
        />
      </div>

      <UsageSection scope={scope} defaultDim="model" />

      {/* Context distribution */}
      <ContextDistributionCard>
        <Frame r={composition} height="" empty={(composition.data?.sources.length ?? 0) === 0}>
          {composition.data ? (
            <ContextSunburst data={composition.data} centerLabel="across windows" />
          ) : null}
        </Frame>
      </ContextDistributionCard>

      <PerformanceSection scope={scope} defaultDim="model" />
      <SuccessSection scope={scope} defaultDim="model" />
      <CacheSection scope={scope} defaultDim="model" />

      <BehavioralSection
        stats={ss}
        loading={sessions.initialLoading}
        windowLabel={windowLabel}
        hint={
          filtered
            ? 'Sessions that used the selected models, providers or clients — each run counted in full'
            : 'How agent runs behave — per-session turns, duration, tokens, and tool use'
        }
      />

      {/* Recent activity — ActivityFeed renders its own title row + sort tabs.
          Rows aren't clickable for a user key (no detail routes/APIs). */}
      <ActivityFeed
        since={scope.since}
        until={scope.until}
        filter={scope.filter}
        interactive={!isUser}
      />
    </Page>
  );
}
