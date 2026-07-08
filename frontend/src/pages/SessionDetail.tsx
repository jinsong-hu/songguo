import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, GitBranch } from 'lucide-react';
import { Area, AreaChart, CartesianGrid, ReferenceLine, XAxis, YAxis } from 'recharts';
import { svg as claudeCodeSvg } from 'thesvg/claude-code';
import { svg as codexOpenAISvg } from 'thesvg/codex-openai';
import { api } from '../api/client';
import type { AgentNode, CallEntry, CallTrace } from '../api/types';
import { ContextSunburst, srcColor, srcLabel } from '../components/ContextSunburst';
import { InfoHint } from '../components/InfoHint';
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
import { dateTime, duration, int, money } from '../lib/format';
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
  const sessionClient = useMemo(() => dominantClient(data?.entries ?? []), [data]);
  const mainPromptEntry = useMemo(() => data?.entries.find(isMainPromptEntry) ?? null, [data]);

  return (
    <Page
      title={
        data ? (
          <span className={styles.sessionTitle}>
            <span>Session</span>
            <code className={`mono ${styles.sessionTitleId}`}>{data.session_id}</code>
            <CopyButton value={data.session_id} ariaLabel="Copy session id" />
          </span>
        ) : (
          'Session'
        )
      }
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
          <div className={styles.kpiRow}>
            {sessionClient ? <ClientTile client={sessionClient} /> : null}
            <Kpi
              label="Tokens"
              value={int(data.input_tokens + data.output_tokens)}
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

          {mainPromptEntry ? <InitialPromptCard entry={mainPromptEntry} /> : null}

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

          {snapshot.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel} style={{ marginBottom: 12, display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                Composition — latest turn
                <InfoHint />
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
  const label = client.name === 'claude-code' ? 'Claude Code' : client.name === 'codex-openai' ? 'Codex' : client.name;
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

function clientIconSvg(name: string): string {
  if (name === 'claude-code') return claudeCodeSvg;
  if (name === 'codex-openai') return codexOpenAISvg;
  return '';
}

function InitialPromptCard({ entry }: { entry: CallEntry }) {
  const trace = useFetch<CallTrace>(() => api.trace(entry.id), [entry.id], {
    enabled: entry.has_trace,
  });
  const prompt = useMemo(() => (trace.data ? parseInitialPrompt(trace.data) : null), [trace.data]);

  if (trace.error) return <ErrorBanner message={trace.error} onRetry={trace.refetch} />;

  if (trace.initialLoading || !prompt) {
    return (
      <div className={styles.promptGrid}>
        <Skeleton height={180} />
        <Skeleton height={180} />
      </div>
    );
  }

  return (
    <div className={styles.promptGrid}>
      <section className={styles.promptPanel}>
        <div className={styles.promptPanelHead}>
          <span>System Prompt</span>
          <span className="chip chip-mono">{prompt.system.length} {prompt.system.length === 1 ? 'block' : 'blocks'}</span>
        </div>
        <div className={styles.promptScroll}>
          {prompt.system.length > 0 ? (
            prompt.system.map((block, i) => (
              <pre key={i} className={styles.promptTextBlock}>{block}</pre>
            ))
          ) : (
            <div className={styles.promptEmpty}>No system content in this request.</div>
          )}
        </div>
      </section>

      <section className={styles.promptPanel}>
        <div className={styles.promptPanelHead}>
          <span>Tools</span>
          <span className="chip chip-mono">{prompt.tools.length}</span>
        </div>
        <div className={styles.toolList}>
          {prompt.tools.length > 0 ? (
            prompt.tools.map((tool, i) => (
              <details key={`${tool.name}-${i}`} className={styles.toolItem}>
                <summary className={styles.toolSummary}>
                  <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
                  <span className={styles.toolMeta}>{tool.propertyCount} inputs</span>
                </summary>
                {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
                <pre className={styles.schemaCode}>{tool.schema}</pre>
              </details>
            ))
          ) : (
            <div className={styles.promptEmpty}>No tools in this request.</div>
          )}
        </div>
      </section>
    </div>
  );
}

interface InitialPrompt {
  system: string[];
  tools: ToolInfo[];
}

interface ToolInfo {
  name: string;
  description: string;
  propertyCount: number;
  schema: string;
}

function isMainPromptEntry(entry: CallEntry): boolean {
  return entry.has_trace && entry.wire !== 'anthropic/count_tokens' && !isHaikuAuxiliary(entry);
}

function isHaikuAuxiliary(entry: CallEntry): boolean {
  return entry.wire === 'anthropic/messages' && entry.model.toLowerCase().includes('haiku');
}

function parseInitialPrompt(trace: CallTrace): InitialPrompt {
  const body = parseJsonObject(trace.request.body);
  if (!body) return { system: [], tools: [] };
  return {
    system: systemBlocks(body.system ?? body.instructions),
    tools: toolInfos(body.tools),
  };
}

function systemBlocks(system: unknown): string[] {
  if (typeof system === 'string') return [system];
  if (Array.isArray(system)) {
    return system.map((block) => {
      if (isRecord(block) && typeof block.text === 'string') return block.text;
      return prettyJson(stripCacheControl(block));
    });
  }
  if (system == null) return [];
  return [prettyJson(stripCacheControl(system))];
}

function toolInfos(tools: unknown): ToolInfo[] {
  if (!Array.isArray(tools)) return [];
  return tools.map((tool) => {
    const record = isRecord(tool) ? tool : {};
    const schema = record.input_schema;
    return {
      name: typeof record.name === 'string' ? record.name : '',
      description: typeof record.description === 'string' ? record.description : '',
      propertyCount: schemaPropertyCount(schema),
      schema: prettyJson(stripCacheControl(schema ?? {})),
    };
  });
}

function schemaPropertyCount(schema: unknown): number {
  if (!isRecord(schema) || !isRecord(schema.properties)) return 0;
  return Object.keys(schema.properties).length;
}

function parseJsonObject(raw: string): Record<string, unknown> | null {
  try {
    const parsed: unknown = JSON.parse(raw);
    return isRecord(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function prettyJson(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

function stripCacheControl(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripCacheControl);
  if (!isRecord(value)) return value;
  const out: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(value)) {
    if (key !== 'cache_control') out[key] = stripCacheControl(child);
  }
  return out;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
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
