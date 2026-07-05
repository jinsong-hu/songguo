import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, GitBranch } from 'lucide-react';
import { Area, AreaChart, CartesianGrid, ReferenceLine, XAxis, YAxis } from 'recharts';
import { api } from '../api/client';
import type { AgentNode } from '../api/types';
import { ContextSunburst, srcColor, srcLabel } from '../components/ContextSunburst';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { StatusPill } from '../components/StatusPill';
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '../components/ui/chart';
import { useFetch } from '../lib/useFetch';
import { dateTime, int, money, ms } from '../lib/format';
import styles from './Detail.module.css';

const SRC_ORDER = ['tool_results', 'tool_schemas', 'system', 'reasoning', 'actions', 'user', 'attachments', 'unattributed'];

export function SessionDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const { data, error, initialLoading, refetch } = useFetch(
    () => api.session(id),
    [id],
    { enabled: id !== '' },
  );

  const ctx = useFetch(() => api.sessionContext(id), [id], { enabled: id !== '' });
  const [axis, setAxis] = useState<'source' | 'cache'>('source');

  const turns = useMemo(() => ctx.data?.turns ?? [], [ctx.data]);
  const snapshot = ctx.data?.snapshot ?? [];
  const lastTotal = turns.length ? turns[turns.length - 1].total : 0;

  const sourceKeys = useMemo(() => {
    const seen = new Set<string>();
    for (const t of turns) for (const k of Object.keys(t.sources)) seen.add(k);
    return SRC_ORDER.filter((k) => seen.has(k)).concat([...seen].filter((k) => !SRC_ORDER.includes(k)));
  }, [turns]);

  const growthData = useMemo(
    () =>
      turns.map((t, i) => {
        const row: Record<string, number | string> = { label: `t${i + 1}` };
        if (axis === 'source') {
          for (const k of sourceKeys) row[k] = Math.round(t.sources[k] ?? 0);
        } else {
          row.reused = Math.round(t.cached);
          row.fresh = Math.max(0, Math.round(t.total - t.cached));
        }
        return row;
      }),
    [turns, sourceKeys, axis],
  );

  const growthConfig = useMemo<ChartConfig>(() => {
    if (axis === 'cache')
      return {
        reused: { label: 'Reused (cached)', color: 'var(--accent)' },
        fresh: { label: 'Fresh (paid)', color: 'var(--amber)' },
      };
    const c: ChartConfig = {};
    for (const k of sourceKeys) c[k] = { label: srcLabel(k), color: srcColor(k) };
    return c;
  }, [axis, sourceKeys]);

  const spanMs = data ? new Date(data.last_ts).getTime() - new Date(data.first_ts).getTime() : 0;

  return (
    <Page
      title="Session"
      actions={
        <Link to="/" className="btn">
          <ArrowLeft size={15} /> Back to overview
        </Link>
      }
    >
      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading || !data ? (
        <div className={styles.stack}>
          <Skeleton height={90} />
          <Skeleton height={140} />
          <Skeleton height={220} />
        </div>
      ) : (
        <div className={styles.stack}>
          <div className={styles.traceHead}>
            <code className="mono" style={{ fontSize: 13 }}>
              {data.session_id}
            </code>
            <CopyButton value={data.session_id} label="Copy session id" />
          </div>

          <div className={styles.kpiRow}>
            <Kpi label="Calls" value={int(data.calls)} />
            <Kpi label="Cost" value={money(data.cost)} />
            <Kpi label="Input tokens" value={int(data.input_tokens)} />
            <Kpi label="Output tokens" value={int(data.output_tokens)} />
            <Kpi label="Errors" value={int(data.error_count)} />
            <Kpi label="Span" value={ms(spanMs)} />
          </div>

          {turns.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.ctxHead}>
                <div className={styles.fieldLabel}>Context growth</div>
                <div className={styles.seg} role="tablist" aria-label="Colour by">
                  {(['source', 'cache'] as const).map((a) => (
                    <button
                      key={a}
                      role="tab"
                      aria-selected={a === axis}
                      className={`${styles.segBtn} ${a === axis ? styles.segActive : ''}`}
                      onClick={() => setAxis(a)}
                    >
                      {a === 'source' ? 'Source' : 'Cache'}
                    </button>
                  ))}
                </div>
              </div>
              <div className={styles.ctxChart}>
                <ChartContainer config={growthConfig} className="aspect-auto h-full w-full">
                  <AreaChart data={growthData} margin={{ top: 8, right: 12, left: 0, bottom: 0 }}>
                    <CartesianGrid vertical={false} />
                    <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={24} />
                    <YAxis tickLine={false} axisLine={false} width={46} tickFormatter={(v: number) => compactTokens(v)} />
                    <ChartTooltip content={<ChartTooltipContent />} />
                    <ChartLegend content={<ChartLegendContent />} />
                    <ReferenceLine y={150000} stroke="var(--amber)" strokeDasharray="5 4" />
                    {(axis === 'source' ? sourceKeys : ['reused', 'fresh']).map((k) => (
                      <Area
                        key={k}
                        dataKey={k}
                        stackId="s"
                        type="monotone"
                        stroke={`var(--color-${k})`}
                        fill={`var(--color-${k})`}
                        fillOpacity={0.72}
                        strokeWidth={1}
                      />
                    ))}
                  </AreaChart>
                </ChartContainer>
              </div>
            </div>
          )}

          {snapshot.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel} style={{ marginBottom: 12 }}>
                Composition — latest turn
              </div>
              <ContextSunburst data={{ avg_total: lastTotal, sources: snapshot }} centerLabel="window" />
            </div>
          )}

          <div className="card" style={{ padding: 16 }}>
            <div className={styles.fieldLabel}>Models</div>
            <div className={styles.tags}>
              {data.models.length > 0
                ? data.models.map((m) => (
                    <span key={m} className="chip chip-mono">
                      {m}
                    </span>
                  ))
                : '—'}
            </div>
          </div>

          {data.agents.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel} style={{ marginBottom: 12 }}>
                <GitBranch size={13} style={{ verticalAlign: -2, marginRight: 6 }} />
                Agent tree
              </div>
              <div className={styles.treeChildren}>
                {data.agents.map((node) => (
                  <AgentTreeNode key={node.agent_id || '(root)'} node={node} />
                ))}
              </div>
            </div>
          )}

          <div className={`card ${styles.stack}`} style={{ padding: 0 }}>
            {data.entries.length === 0 ? (
              <EmptyState title="No calls in this session" />
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table className="table">
                  <thead>
                    <tr>
                      <th>Time</th>
                      <th>Model</th>
                      <th>Vendor</th>
                      <th>Agent</th>
                      <th className="num">Tokens</th>
                      <th className="num">Cost</th>
                      <th>Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.entries.map((e) => (
                      <tr
                        key={e.id}
                        className={styles.clickRow}
                        onClick={() => navigate(`/calls/${e.id}`)}
                      >
                        <td className="mono" style={{ color: 'var(--text-muted)' }}>
                          {dateTime(e.ts)}
                        </td>
                        <td className="mono">{e.model || '—'}</td>
                        <td>{e.vendor || '—'}</td>
                        <td className="mono" style={{ fontSize: 11.5 }}>
                          {shortAgent(e.agent_id)}
                        </td>
                        <td className="num">{int(e.input_tokens + e.output_tokens)}</td>
                        <td className="num">{money(e.cost)}</td>
                        <td>
                          <StatusPill status={e.status} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}
    </Page>
  );
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className={`card ${styles.kpi}`}>
      <div className={styles.fieldLabel}>{label}</div>
      <div className={styles.kpiValue}>{value}</div>
    </div>
  );
}

function AgentTreeNode({ node }: { node: AgentNode }) {
  return (
    <div className={styles.treeNode}>
      <div className={styles.treeRow}>
        <span className={styles.treeAgent}>{node.agent_id || '(main)'}</span>
        <span className={styles.treeMeta}>
          {int(node.calls)} calls · {money(node.cost)} · {int(node.input_tokens + node.output_tokens)} tok
        </span>
      </div>
      {node.children.length > 0 && (
        <div className={styles.treeChildren}>
          {node.children.map((child) => (
            <AgentTreeNode key={child.agent_id} node={child} />
          ))}
        </div>
      )}
    </div>
  );
}

function shortAgent(id: string): string {
  if (!id) return '—';
  return id.length > 10 ? `${id.slice(0, 10)}…` : id;
}

function compactTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`;
  return `${Math.round(n)}`;
}
