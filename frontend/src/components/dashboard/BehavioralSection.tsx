import { Clock, Coins, GitBranch, MessageSquare, Wrench } from 'lucide-react';
import type { SessionStats } from '../../api/types';
import { duration, int } from '../../lib/format';
import { compact } from '../../lib/seriesColors';
import { Kpi, SectionTitle, dashboardStyles as styles } from './Section';

/**
 * Per-session behaviour: turns, duration, tokens and tool use.
 *
 * The one section where the top-bar filter changes meaning rather than just
 * narrowing: a session is a whole agent run, not divisible by model, so
 * filtering picks the runs that *used* a selected model, provider or client and
 * still reports each run in full. The caller says so via `hint` when a filter is
 * on.
 *
 * It takes the stats rather than fetching them because the page usually needs
 * the same response for its own KPI row; two fetches of one endpoint per tick
 * would be pure waste.
 */
export function BehavioralSection({
  stats,
  loading,
  windowLabel,
  hint,
  title = 'Behavioral',
}: {
  stats: SessionStats | null;
  loading: boolean;
  /** Human name of the current window, e.g. "Last 24 hours". */
  windowLabel: string;
  hint?: string;
  title?: string;
}) {
  const ss = stats;
  return (
    <>
      <SectionTitle name={title} hint={hint} />
      <div className={styles.kpiGrid}>
        <Kpi
          icon={<GitBranch size={14} />}
          label={`Sessions (${windowLabel})`}
          loading={loading}
          value={ss ? int(ss.sessions) : '—'}
          sub={ss ? `${int(ss.with_subagents)} used subagents` : undefined}
        />
        <Kpi
          icon={<MessageSquare size={14} />}
          label="Turns / session"
          loading={loading}
          value={ss ? ss.avg_turns.toFixed(1) : '—'}
          sub={ss ? `p50 ${int(ss.turns_p50)} · p95 ${int(ss.turns_p95)}` : undefined}
        />
        <Kpi
          icon={<Clock size={14} />}
          label="Duration / session"
          loading={loading}
          value={ss ? duration(ss.avg_duration) : '—'}
          sub={ss ? `p50 ${duration(ss.duration_p50)} · p95 ${duration(ss.duration_p95)}` : undefined}
        />
        <Kpi
          icon={<Coins size={14} />}
          label="Tokens / session"
          loading={loading}
          value={ss ? compact(ss.avg_tokens) : '—'}
          sub={ss ? `p50 ${compact(ss.tokens_p50)} · p95 ${compact(ss.tokens_p95)}` : undefined}
        />
        <Kpi
          icon={<Wrench size={14} />}
          label="Tools / session"
          loading={loading}
          value={ss ? ss.avg_tool_calls.toFixed(1) : '—'}
          sub={ss ? `p50 ${int(ss.tool_calls_p50)} · p95 ${int(ss.tool_calls_p95)}` : undefined}
        />
      </div>
    </>
  );
}
