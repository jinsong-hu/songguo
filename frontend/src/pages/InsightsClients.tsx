import { Coins, DatabaseZap, DollarSign, MonitorSmartphone } from 'lucide-react';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { BehavioralSection } from '../components/dashboard/BehavioralSection';
import { CacheSection } from '../components/dashboard/CacheSection';
import { PerformanceSection } from '../components/dashboard/PerformanceSection';
import { SuccessSection } from '../components/dashboard/SuccessSection';
import { UsageSection } from '../components/dashboard/UsageSection';
import { Kpi, SectionTitle, dashboardStyles as styles } from '../components/dashboard/Section';
import { useDashboard } from '../components/dashboard/useDashboard';
import { int, money, percent } from '../lib/format';
import { compact } from '../lib/seriesColors';

/**
 * The gateway seen from the calling agent's side: how each coding agent uses it.
 *
 * A caveat this page states out loud rather than burying: the per-session
 * numbers here are NOT a ranking of clients. Different agents run different
 * work, so a higher cost per session may only mean the harder task. What *is*
 * comparable is how a client shapes its requests — above all its cache hit rate,
 * which is a property of how it builds prompts rather than of what it was asked
 * to do.
 *
 * Note the traffic here is not exhaustive: calls whose User-Agent named no
 * recognized client group under "unknown" (curl, raw SDKs, health checks).
 */
export function InsightsClientsPage() {
  const { scope, toolbar, overview, sessions, facets, filtered, windowLabel } = useDashboard({
    dimensions: ['client', 'model', 'vendor', 'user'],
    isUser: false,
  });

  const ov = overview.data;
  const ss = sessions.data;
  const cacheHit =
    ov && ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation > 0
      ? ov.tokens.cached / (ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation)
      : null;
  const costPerSession = ov && ss && ss.sessions > 0 ? ov.total_spend / ss.sessions : null;

  return (
    <Page title="Clients" actions={toolbar}>
      {overview.error && (
        <div style={{ marginBottom: 16 }}>
          <ErrorBanner message={overview.error} onRetry={overview.refetch} />
        </div>
      )}

      <div className={styles.kpiGrid}>
        <Kpi
          icon={<MonitorSmartphone size={14} />}
          label="Recognized clients"
          loading={facets.initialLoading}
          value={facets.data ? int(facets.data.clients.length) : '—'}
          sub="plus unrecognized callers"
        />
        <Kpi
          icon={<DatabaseZap size={14} />}
          label="Cache hit"
          loading={overview.initialLoading}
          value={cacheHit == null ? '—' : percent(cacheHit)}
        />
        {/* Per-session tokens and tool calls belong to the Behavioral section
            below, which reports them with percentiles — repeating the means up
            here just made the same number appear twice on one screen. */}
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens"
          loading={overview.initialLoading}
          value={
            ov
              ? compact(ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation + ov.tokens.output)
              : '—'
          }
          sub={ov ? `${int(ov.requests)} calls` : undefined}
        />
        <Kpi
          icon={<DollarSign size={14} />}
          label="Cost / session"
          loading={overview.initialLoading || sessions.initialLoading}
          value={costPerSession == null ? '—' : money(costPerSession)}
          sub={ss ? `over ${int(ss.sessions)} sessions` : undefined}
        />
      </div>

      {/* Stated on the page, not just in the source: these numbers describe
          clients, they do not rank them. */}
      <SectionTitle
        name="How each client builds its requests"
        hint="Comparable between clients — this is prompt shape, not workload"
      />
      <CacheSection scope={scope} defaultDim="client" title="Cache" />
      <UsageSection scope={scope} defaultDim="client" title="Tokens & cost" />

      <SectionTitle
        name="What each client was asked to do"
        hint="Depends on the task, so a higher number is not a worse client"
      />
      <BehavioralSection
        stats={ss}
        loading={sessions.initialLoading}
        windowLabel={windowLabel}
        hint={
          filtered
            ? 'Sessions that used the selected models, providers or clients — each run counted in full'
            : 'Per-session turns, duration, tokens, and tool use'
        }
        title="Per session"
      />

      <PerformanceSection scope={scope} defaultDim="client" />
      <SuccessSection scope={scope} defaultDim="client" />
    </Page>
  );
}
