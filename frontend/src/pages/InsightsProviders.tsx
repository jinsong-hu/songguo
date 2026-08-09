import { Activity, DatabaseZap, DollarSign, Plug, ShieldCheck, Timer } from 'lucide-react';
import { api } from '../api/client';
import { Page } from '../components/Layout';
import { CacheSection } from '../components/dashboard/CacheSection';
import { PerformanceSection } from '../components/dashboard/PerformanceSection';
import { SuccessSection } from '../components/dashboard/SuccessSection';
import { UsageSection } from '../components/dashboard/UsageSection';
import { Kpi, dashboardStyles as styles } from '../components/dashboard/Section';
import { useDashboard } from '../components/dashboard/useDashboard';
import { ErrorBanner } from '../components/ErrorBanner';
import { useFetch } from '../lib/useFetch';
import { int, money, percent } from '../lib/format';

/**
 * The gateway seen from the provider side: who is carrying the traffic, what it
 * costs, how fast it answers and how often it fails.
 *
 * Same sections as the Overview, defaulted to the by-provider breakdown, minus
 * the ones that say nothing about a provider (context composition is a property
 * of the prompt; per-session behaviour is a property of the agent). The one
 * addition is the routing-state KPI, which is the only number here that is live
 * rather than ledger history.
 */
export function InsightsProvidersPage() {
  const { scope, toolbar, overview } = useDashboard({
    dimensions: ['vendor', 'model', 'client', 'user'],
    isUser: false,
  });

  // Live router state. Distinct from everything else on the page: the ledger
  // says what happened, this says what the *next* request will do, and the two
  // can legitimately disagree.
  const vendors = useFetch(() => api.vendors(), [], { intervalMs: scope.intervalMs });

  const ov = overview.data;
  const cacheHit =
    ov && ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation > 0
      ? ov.tokens.cached / (ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation)
      : null;

  const routing = (vendors.data ?? []).reduce(
    (acc, v) => {
      if (v.routing?.dead) acc.dead += 1;
      else if (v.routing?.cooling) acc.cooling += 1;
      else acc.live += 1;
      acc.waiting += v.capacity?.waiting ?? 0;
      return acc;
    },
    { live: 0, cooling: 0, dead: 0, waiting: 0 },
  );
  const degraded = routing.cooling + routing.dead;

  return (
    <Page title="Providers" actions={toolbar}>
      {overview.error && (
        <div style={{ marginBottom: 16 }}>
          <ErrorBanner message={overview.error} onRetry={overview.refetch} />
        </div>
      )}

      <div className={styles.kpiGrid}>
        <Kpi
          icon={<Plug size={14} />}
          label="Providers with traffic"
          loading={overview.initialLoading}
          value={ov ? int(ov.vendors_active) : '—'}
        />
        <Kpi
          icon={<Activity size={14} />}
          label="Routing state"
          loading={vendors.initialLoading}
          // Live state, not history — a provider that failed all week may be
          // perfectly routable right now, and vice versa.
          value={vendors.data ? `${int(routing.live)} live` : '—'}
          sub={
            degraded > 0
              ? `${int(routing.cooling)} cooling · ${int(routing.dead)} dead`
              : 'all healthy'
          }
          danger={routing.dead > 0}
        />
        <Kpi
          icon={<Timer size={14} />}
          label="Queued for a slot"
          loading={vendors.initialLoading}
          // Persistently non-zero means a max_concurrency is set below what the
          // provider actually allows. It never reroutes — it waits.
          value={vendors.data ? int(routing.waiting) : '—'}
        />
        <Kpi
          icon={<DollarSign size={14} />}
          label="Cost"
          loading={overview.initialLoading}
          value={ov ? money(ov.total_spend) : '—'}
          sub={ov ? `${int(ov.requests)} calls` : undefined}
        />
        <Kpi
          icon={<ShieldCheck size={14} />}
          label="Success rate"
          loading={overview.initialLoading}
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

      <SuccessSection scope={scope} defaultDim="vendor" title="Reliability" />
      <PerformanceSection scope={scope} defaultDim="vendor" />
      <UsageSection scope={scope} defaultDim="vendor" title="Traffic & cost" />
      <CacheSection scope={scope} defaultDim="vendor" />
    </Page>
  );
}
