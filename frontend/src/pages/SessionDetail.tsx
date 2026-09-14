import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { Area, AreaChart, CartesianGrid, ReferenceLine, XAxis, YAxis } from 'recharts';
import { api } from '../api/client';
import type { CallEntry, ContextBlock, SourceSlice } from '../api/types';
import { ContextSunburst, ContextDistributionCard, srcColor, srcLabel, type ContextSelection } from '../components/ContextSunburst';
import { InfoHint } from '../components/InfoHint';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import {
  PromptReconstructionCard,
  jumpToPromptBlock,
  parsePromptReconstruction,
  snippet,
  type PromptBlock,
} from '../components/PromptReconstruction';
import { SessionTimeline } from '../components/SessionTimeline';
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
import { clientIconSvg, clientLabel } from '../lib/client';
import { dateTime, duration, elapsedSince, int, money } from '../lib/format';
import styles from './Detail.module.css';

const SRC_ORDER = [
  'tool_results', 'tool_calls', 'tool_schemas', 'system', 'assistant', 'user', 'other',
  // Legacy source keys — historical rows only.
  'reasoning', 'actions', 'attachments', 'unattributed',
];

export function SessionDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const { data, error, initialLoading, refetch } = useFetch(
    () => api.session(id),
    [id],
    { enabled: id !== '' },
  );

  const [selectedAgent, setSelectedAgent] = useState('');
  const ctx = useFetch(
    () => api.sessionContext(id, selectedAgent),
    [id, selectedAgent],
    { enabled: id !== '' },
  );
  const [axis, setAxis] = useState<'source' | 'cache'>('source');

  const turns = useMemo(() => ctx.data?.turns ?? [], [ctx.data]);
  // Agent scope for the two context charts. `agents` lists every selectable
  // scope (main first); `scopedAgent` is the one the server actually resolved to
  // (echoed back), so the picker highlight stays correct even when a stale
  // selection falls back to main on a different session.
  const agents = useMemo(() => ctx.data?.agents ?? [], [ctx.data]);
  const scopedAgent = ctx.data?.agent ?? '';
  const distribution = ctx.data?.distribution;
  const distributionTotal = useMemo(() => distribution?.sources.reduce((sum, source) => sum + source.tokens, 0) ?? 0, [distribution]);
  const latestCompositionCallId = turns.length ? turns[turns.length - 1].call_id : null;

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
  const sessionClient = useMemo(() => dominantClient(data?.entries ?? []), [data]);
  const mainPromptEntries = useMemo(() => data?.entries.filter(isMainPromptEntry) ?? [], [data]);
  // Utility ("side-track") calls — monitor, count_tokens, title/compaction — are
  // hidden by default; the toggle mixes them back into the chronological list,
  // marked distinctly. The backend already returns every call oldest-first, so
  // this is a client-side filter, no refetch or sort.
  const [includeUtility, setIncludeUtility] = useState(false);
  const utilityCount = useMemo(
    () => data?.entries.filter(isUtilityEntry).length ?? 0,
    [data],
  );
  const callRows = useMemo(() => {
    const entries = data?.entries ?? [];
    return includeUtility ? entries : entries.filter((e) => !isUtilityEntry(e));
  }, [data, includeUtility]);
  const sessionTitle = data?.title || ctx.data?.title || '';
  const sessionMessages = useFetch(() => api.sessionMessages(id), [id], { enabled: id !== '' });
  const prompt = useMemo(
    () => (sessionMessages.data ? parsePromptReconstruction(sessionMessages.data) : null),
    [sessionMessages.data],
  );
  const [ctxSelection, setCtxSelection] = useState<ContextSelection | null>(null);

  return (
    <Page
      title={
        data ? (
          <>
            <span>Session</span>
            {sessionTitle ? <span className={styles.sessionHeaderTitle}>{sessionTitle}</span> : null}
          </>
        ) : (
          'Session'
        )
      }
      actions={
        data ? (
          <span className={styles.sessionTitle}>
            <code className={`mono ${styles.sessionTitleId}`}>{data.session_id}</code>
            <CopyButton value={data.session_id} ariaLabel="Copy session id" />
          </span>
        ) : null
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
          <div className={styles.kpiRow}>
            {sessionClient ? <ClientTile client={sessionClient} /> : null}
            <Kpi
              label="Tokens"
              value={int(
                data.input_tokens +
                  data.cache_read_input_tokens +
                  data.cache_creation_input_tokens +
                  data.output_tokens,
              )}
              footLabel="Cost"
              footValue={money(data.cost)}
            />
            <Kpi
              label="Duration"
              value={duration(spanMs / 1000)}
              footLabel="Turns"
              footValue={int(turns.length)}
            />
          </div>

          {data.entries.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div
                className={styles.fieldLabel}
                style={{ display: 'inline-flex', alignItems: 'center', gap: 6, marginBottom: 14 }}
              >
                Timeline
                <InfoHint text="Every proxied call on a shared clock. The top lane is the main session; each sub-agent gets its own lane. Bar colour is the call's kind (tool/text turn, monitor, count-tokens, utility, failure). Between calls, no request is in flight: each gap is split into an estimated tool-run band (hatched, one slice per tool the next request carried) plus an idle slice — an even 1/(n+1) split, refined later. The bar below sums where the wall-clock went. Hover for detail." />
              </div>
              <SessionTimeline entries={data.entries} />
            </div>
          )}

          {agents.length > 1 && (
            <div className="card" style={{ padding: 16 }}>
              <div
                className={styles.fieldLabel}
                style={{ display: 'inline-flex', alignItems: 'center', gap: 6, marginBottom: 12 }}
              >
                Context scope
                <InfoHint text="The distribution and growth charts below show one agent at a time. The main session is the default; each sub-agent (a Task/sub-agent run) keeps its own context window, so viewing them separately avoids interleaving their growth. Harness utility calls (monitor, count-tokens, title/compaction) are excluded from both charts." />
              </div>
              <div className={styles.agentScope} role="tablist" aria-label="Context agent scope">
                {agents.map((ag) => {
                  const active = ag.agent_id === scopedAgent;
                  return (
                    <button
                      key={ag.agent_id || 'main'}
                      role="tab"
                      aria-selected={active}
                      className={`${styles.agentChip} ${active ? styles.agentChipActive : ''}`}
                      onClick={() => setSelectedAgent(ag.agent_id)}
                    >
                      {ag.label}
                      <span className={styles.agentChipCount}>{int(ag.turns)}</span>
                    </button>
                  );
                })}
              </div>
            </div>
          )}

          {distribution && distribution.sources.length > 0 && (
            <ContextDistributionCard
              info={
                <InfoHint
                  text="Total tokens across request windows, including repeated context. Calculated with Songguo's local token counter, so counts may differ from official provider counts."
                  content={
                    <>
                      <span>
                        Total tokens across request windows, including repeated context.
                        Calculated with Songguo's local token counter, so counts may differ
                        from official provider counts.
                      </span>
                      {latestCompositionCallId ? (
                        <span style={{ display: 'block', marginTop: 8 }}>
                          For one request window, open the{' '}
                          <Link to={`/calls/${latestCompositionCallId}`}>latest request</Link>.
                        </span>
                      ) : null}
                    </>
                  }
                />
              }
            >
              <ContextSunburst data={distribution} centerValue={distributionTotal} centerLabel="" active={ctxSelection} onSelect={setCtxSelection} />
              <ContextBlockDrilldown selection={ctxSelection} blocks={distribution.blocks ?? []} promptBlocks={prompt?.blocks ?? []} sources={distribution.sources} />
            </ContextDistributionCard>
          )}

          {turns.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.ctxHead}>
                <div className={styles.fieldLabel} style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                  Context growth
                  <InfoHint />
                </div>
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

          {mainPromptEntries.length > 0 ? (
            <PromptReconstructionCard
              prompt={prompt}
              loading={sessionMessages.initialLoading || !prompt}
              error={sessionMessages.error}
              onRetry={sessionMessages.refetch}
            />
          ) : null}

          <div className={`card ${styles.stack}`} style={{ padding: 0 }}>
            {data.entries.length === 0 ? (
              <EmptyState title="No calls in this session" />
            ) : (
              <>
                <div className={styles.callsHeader}>
                  <div className={styles.callsTitle}>
                    Calls <span className={styles.callsCount}>{callRows.length}</span>
                  </div>
                  {utilityCount > 0 ? (
                    <div className={styles.seg} role="group" aria-label="Utility calls">
                      <button
                        type="button"
                        aria-pressed={includeUtility}
                        className={`${styles.segBtn} ${includeUtility ? styles.segActive : ''}`}
                        onClick={() => setIncludeUtility((v) => !v)}
                        title="Harness utility calls: monitor, count-tokens, title/compaction"
                      >
                        Include utility ({utilityCount})
                      </button>
                    </div>
                  ) : null}
                </div>
                <div style={{ overflowX: 'auto' }}>
                  <table className="table">
                    <thead>
                      <tr>
                        <th>Time</th>
                        <th>Wire</th>
                        <th>Model</th>
                        <th className="num">Tokens</th>
                        <th className="num">Cost</th>
                        <th className="num">Duration</th>
                        <th>Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      {callRows.map((e) => {
                        const util = isUtilityEntry(e);
                        return (
                          <tr
                            key={e.id}
                            className={`${styles.clickRow} ${util ? styles.utilityRow : ''}`}
                            onClick={() => navigate(`/calls/${encodeURIComponent(e.id)}`)}
                          >
                            <td className="mono" style={{ color: 'var(--text-muted)' }}>
                              {dateTime(e.ts)}
                            </td>
                            <td className="mono">
                              {util ? (
                                <span className={styles.utilityChip}>{utilityLabel(e.entrypoint)}</span>
                              ) : (
                                e.wire || '—'
                              )}
                            </td>
                            <td className="mono">{e.model || '—'}</td>
                            <td className="num">{int(e.input_tokens + e.cache_read_input_tokens + e.cache_creation_input_tokens + e.output_tokens)}</td>
                            <td className="num">{money(e.cost)}</td>
                            <td className="num">
                              {e.pending ? (
                                <span style={{ color: 'var(--text-muted)' }}>{elapsedSince(e.ts)}</span>
                              ) : (
                                duration(e.latency_ms / 1000)
                              )}
                            </td>
                            <td>
                              <StatusPill call={e} />
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </Page>
  );
}

function Kpi({
  label,
  value,
  footLabel,
  footValue,
}: {
  label: string;
  value: string;
  footLabel?: string;
  footValue?: string;
}) {
  return (
    <div className={`card ${styles.kpi}`}>
      <div className={styles.fieldLabel}>{label}</div>
      <div className={styles.kpiValue}>{value}</div>
      {footLabel || footValue ? (
        <div className={styles.tileFoot}>
          <span className={styles.tileFootLabel}>{footLabel}</span>
          <span className={styles.tileFootValue}>{footValue}</span>
        </div>
      ) : null}
    </div>
  );
}

interface ClientBadgeData {
  name: string;
  version: string;
}

function dominantClient(entries: CallEntry[]): ClientBadgeData | null {
  const counts = new Map<string, { client: ClientBadgeData; count: number }>();
  for (const e of entries) {
    if (!e.client_name) continue;
    const key = `${e.client_name}\x00${e.client_version}`;
    const current = counts.get(key);
    if (current) {
      current.count += 1;
    } else {
      counts.set(key, { client: { name: e.client_name, version: e.client_version }, count: 1 });
    }
  }
  let best: { client: ClientBadgeData; count: number } | null = null;
  for (const item of counts.values()) {
    if (!best || item.count > best.count) best = item;
  }
  return best?.client ?? null;
}

function ClientTile({ client }: { client: ClientBadgeData }) {
  const label = clientLabel(client.name);
  const icon = clientIconSvg(client.name);

  return (
    <div className={`card ${styles.clientTile}`}>
      <div className={styles.fieldLabel}>Client</div>
      <div className={styles.clientIconStage}>
        {icon ? <span className={styles.clientIcon} aria-hidden="true" dangerouslySetInnerHTML={{ __html: icon }} /> : null}
      </div>
      <div className={styles.tileFoot} title={client.version ? `${label} ${client.version}` : label}>
        <span className={styles.tileFootLabel}>{label}</span>
        {client.version ? <span className={styles.tileFootValue}>{client.version}</span> : null}
      </div>
    </div>
  );
}

function ContextBlockDrilldown({
  selection,
  blocks,
  promptBlocks,
  sources,
}: {
  selection: ContextSelection | null;
  blocks: ContextBlock[];
  promptBlocks: PromptBlock[];
  sources: SourceSlice[];
}) {
  if (!selection) {
    return null;
  }

  const matches = blocks
    .filter((block) => block.source === selection.source && (!selection.producer || block.producer === selection.producer))
    .sort((a, b) => b.total - a.total);
  const aggregateTokens = sources.reduce((sum, source) => sum + source.tokens, 0);
  const promptByHash = new Map(promptBlocks.map((block) => [block.hash, block]));
  const blockPct = (tokens: number, total: number) => (total > 0 ? `${((tokens / total) * 100).toFixed(1)}%` : '—');

  return (
    <div className={styles.ctxBlockPanel}>
      {matches.length > 0 ? (
        <div className={styles.ctxBlockTableScroll}>
          <table className={`table ${styles.ctxBlockTable}`}>
            <thead>
              <tr>
                <th>Type</th>
                <th className="num">Tokens</th>
                <th className="num">Occurrences</th>
                <th className="num">Total</th>
                <th className="num">Total %</th>
                <th>Preview</th>
              </tr>
            </thead>
            <tbody>
              {matches.map((block) => {
                const promptBlock = promptByHash.get(block.hash);
                const preview = promptBlock?.snippet ?? '';
                const previewFallback = block.hash.startsWith('source-total:')
                  ? 'Source-level total from the local counter'
                  : 'No captured preview for this counted block';
                const blockType = contextBlockType(block, promptBlock);
                return (
                  <tr key={`${block.source}:${block.producer ?? ''}:${block.hash}`}>
                    <td className={styles.ctxBlockTitleCell} title={blockType}>{blockType}</td>
                    <td className="num">{int(block.tokens)}</td>
                    <td className="num">{int(block.occurrences)}x</td>
                    <td className="num">{int(block.total)}</td>
                    <td className="num">{blockPct(block.total, aggregateTokens)}</td>
                    <td className={styles.ctxBlockSnippetCell} title={preview}>
                      {promptBlock ? (
                        <button
                          type="button"
                          className={styles.ctxBlockPreviewButton}
                          onClick={() => jumpToPromptBlock(promptBlock.id)}
                        >
                          {preview ? snippet(preview) : '—'}
                        </button>
                      ) : (
                        <span className={styles.ctxBlockPreviewText}>{previewFallback}</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <div className={styles.ctxBlockEmpty}>
          No itemized blocks for this slice yet.
        </div>
      )}
    </div>
  );
}

function contextBlockType(block: ContextBlock, promptBlock?: PromptBlock): string {
  if (block.type === 'Text block') {
    if (block.source === 'system') return 'System text';
    if (block.source === 'user') return 'User text';
    if (block.source === 'assistant' || block.source === 'actions') return 'Assistant text';
  }
  return promptBlock?.title ?? block.type;
}

function isMainPromptEntry(entry: CallEntry): boolean {
  return entry.has_trace && entry.wire !== 'anthropic/count_tokens';
}

// isUtilityEntry reports whether a call is a harness utility call (monitor,
// count_tokens, title/compaction) rather than a visible main-loop turn. Mirrors
// the backend calls.Entrypoint.IsUtility(): empty (legacy) reads as main.
function isUtilityEntry(entry: CallEntry): boolean {
  return entry.entrypoint !== '' && entry.entrypoint !== 'main';
}

// utilityLabel is the short chip text for a utility call's kind.
function utilityLabel(entrypoint: string): string {
  switch (entrypoint) {
    case 'monitor':
      return 'monitor';
    case 'count_tokens':
      return 'count tokens';
    default:
      return 'utility';
  }
}


function compactTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`;
  return `${Math.round(n)}`;
}
