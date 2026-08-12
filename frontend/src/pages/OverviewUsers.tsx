import { CalendarClock, DatabaseZap, DollarSign, Flame, ShieldCheck, Users } from 'lucide-react';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { BehavioralSection } from '../components/dashboard/BehavioralSection';
import { CacheSection } from '../components/dashboard/CacheSection';
import { SuccessSection } from '../components/dashboard/SuccessSection';
import { UsageSection } from '../components/dashboard/UsageSection';
import { Kpi, dashboardStyles as styles } from '../components/dashboard/Section';
import { useDashboard } from '../components/dashboard/useDashboard';
import { int, money, percent } from '../lib/format';

/**
 * The gateway seen from the consumer side: who is spending, on what, and how
 * long the budget lasts at the current rate.
 *
 * The Users page under Settings manages keys — this one is about their traffic.
 * Cache hit is here rather than only on Models because it is the biggest per-key
 * cost lever there is: two keys running the same model can differ several-fold
 * on spend purely by how well their prompts cache.
 */
export function OverviewUsersPage() {
  const { scope, toolbar, overview, sessions, filtered, windowLabel } = useDashboard({
    dimensions: ['user', 'model', 'vendor', 'client'],
    isUser: false,
  });

  const ov = overview.data;
  const cacheHit =
    ov && ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation > 0
      ? ov.tokens.cached / (ov.tokens.input + ov.tokens.cached + ov.tokens.cache_creation)
      : null;

  return (
    <Page title="Users" actions={toolbar}>
      {overview.error && (
        <div style={{ marginBottom: 16 }}>
          <ErrorBanner message={overview.error} onRetry={overview.refetch} />
        </div>
      )}

      <div className={styles.kpiGrid}>
        <Kpi
          icon={<Users size={14} />}
          label="Active users"
          loading={overview.initialLoading}
          value={ov ? int(ov.active_callers) : '—'}
          sub={ov ? `${int(ov.requests)} calls` : undefined}
        />
        <Kpi
          icon={<DollarSign size={14} />}
          label="Cost"
          loading={overview.initialLoading}
          value={ov ? money(ov.total_spend) : '—'}
        />
        <Kpi
          icon={<Flame size={14} />}
          label="Daily burn"
          loading={overview.initialLoading}
          value={ov ? money(ov.daily_burn) : '—'}
        />
        <Kpi
          icon={<CalendarClock size={14} />}
          label="Runway"
          loading={overview.initialLoading}
          // Null when no budget is set or nothing is being spent — there is no
          // runway to report, which is not the same as zero days left.
          value={ov?.runway_days == null ? '—' : `${Math.round(ov.runway_days)}d`}
          sub={ov?.runway_days == null ? 'no budget set' : 'at the current burn'}
          danger={ov?.runway_days != null && ov.runway_days < 7}
        />
        <Kpi
          icon={<ShieldCheck size={14} />}
          label="Success rate"
          loading={overview.initialLoading}
          value={ov ? (ov.rated > 0 ? percent(1 - ov.error_rate) : '—') : '—'}
          // Budget and rate-limit refusals land on this page more than any
          // other, and they are graded in neither side of the rate — so name
          // them beside it rather than letting the two look inconsistent.
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

      <UsageSection scope={scope} defaultDim="user" title="Spend & tokens" />
      <CacheSection scope={scope} defaultDim="user" />
      <SuccessSection scope={scope} defaultDim="user" title="Errors & refusals" />
      <BehavioralSection
        stats={sessions.data}
        loading={sessions.initialLoading}
        windowLabel={windowLabel}
        hint={
          filtered
            ? 'Sessions that used the selected models, providers or clients — each run counted in full'
            : 'How these callers’ agent runs behave — per-session turns, duration, tokens, and tool use'
        }
      />
    </Page>
  );
}
