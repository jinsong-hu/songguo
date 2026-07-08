import { useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, ChevronRight, GitBranch } from 'lucide-react';
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
      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            System Prompt
          </span>
          <span className="chip chip-mono">{prompt.system.length}</span>
        </summary>
        <div className={styles.promptScroll}>
          {prompt.system.length > 0 ? (
            prompt.system.map((block, i) => (
              <pre key={i} className={styles.promptTextBlock}>{block}</pre>
            ))
          ) : (
            <div className={styles.promptEmpty}>No system content in this request.</div>
          )}
        </div>
      </details>

      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            Tools
          </span>
          <span className="chip chip-mono">{prompt.tools.length}</span>
        </summary>
        <div className={styles.toolList}>
          {prompt.tools.length > 0 ? (
            prompt.tools.map((tool, i) => (
              <ToolCard key={`${tool.name}-${i}`} tool={tool} />
            ))
          ) : (
            <div className={styles.promptEmpty}>No tools in this request.</div>
          )}
        </div>
      </details>
    </div>
  );
}

function ToolCard({ tool }: { tool: ToolInfo }) {
  const ref = useRef<HTMLDetailsElement | null>(null);
  const [align, setAlign] = useState<'left' | 'right'>('left');

  function handleToggle() {
    const el = ref.current;
    if (!el?.open) return;
    const rect = el.getBoundingClientRect();
    const viewportWidth = window.innerWidth;
    const panelWidth = Math.min(760, viewportWidth - 80);
    const margin = 24;
    const overflowsRight = rect.left + panelWidth > viewportWidth - margin;
    const fitsLeft = rect.right - panelWidth >= margin;
    setAlign(overflowsRight && fitsLeft ? 'right' : 'left');
  }

  return (
    <details ref={ref} className={styles.toolCard} onToggle={handleToggle}>
      <summary className={styles.toolSummary}>
        <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
        <span className={styles.toolMeta}>{tool.propertyCount} inputs</span>
      </summary>
      <div className={`${styles.toolDetail} ${align === 'right' ? styles.toolDetailRight : styles.toolDetailLeft}`}>
        {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
        <SchemaView schema={tool.schema} />
      </div>
    </details>
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
  schema: unknown;
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
    const fn = isRecord(record.function) ? record.function : {};
    const schema = record.input_schema ?? record.parameters ?? fn.parameters;
    return {
      name:
        typeof record.name === 'string'
          ? record.name
          : typeof fn.name === 'string'
            ? fn.name
            : typeof record.type === 'string'
              ? record.type
              : '',
      description:
        typeof record.description === 'string'
          ? record.description
          : typeof fn.description === 'string'
            ? fn.description
            : '',
      propertyCount: schemaPropertyCount(schema),
      schema: stripCacheControl(schema ?? {}),
    };
  });
}

function SchemaView({ schema }: { schema: unknown }) {
  const fields = schemaFields(schema);
  if (fields.length === 0) {
    return (
      <div className={styles.schemaEmpty}>
        <span>No structured inputs.</span>
        {isRecord(schema) && Object.keys(schema).length > 0 ? (
          <details className={styles.schemaRaw}>
            <summary>Raw schema</summary>
            <pre className={styles.schemaCode}>{prettyJson(schema)}</pre>
          </details>
        ) : null}
      </div>
    );
  }
  return (
    <div className={styles.schemaFields}>
      {fields.map((field) => (
        <SchemaField key={field.path} field={field} />
      ))}
    </div>
  );
}

interface SchemaFieldInfo {
  path: string;
  name: string;
  type: string;
  description: string;
  required: boolean;
  enumValues: string[];
  children: SchemaFieldInfo[];
}

function SchemaField({ field }: { field: SchemaFieldInfo }) {
  return (
    <div className={styles.schemaField}>
      <div className={styles.schemaFieldTop}>
        <span className={styles.schemaFieldName}>{field.name}</span>
        <span className={styles.schemaFieldType}>{field.type}</span>
        <span className={field.required ? styles.schemaRequired : styles.schemaOptional}>
          {field.required ? 'Required' : 'Optional'}
        </span>
      </div>
      {field.description ? <div className={styles.schemaFieldDesc}>{field.description}</div> : null}
      {field.enumValues.length > 0 ? (
        <div className={styles.schemaEnums}>
          {field.enumValues.map((value) => (
            <span key={value} className={styles.schemaEnum}>{value}</span>
          ))}
        </div>
      ) : null}
      {field.children.length > 0 ? (
        <div className={styles.schemaChildren}>
          {field.children.map((child) => (
            <SchemaField key={child.path} field={child} />
          ))}
        </div>
      ) : null}
    </div>
  );
}

function schemaFields(schema: unknown): SchemaFieldInfo[] {
  if (!isRecord(schema) || !isRecord(schema.properties)) return [];
  const required = stringSet(schema.required);
  return Object.entries(schema.properties).map(([name, value]) =>
    schemaFieldInfo(name, value, required.has(name), name),
  );
}

function schemaFieldInfo(name: string, schema: unknown, required: boolean, path: string): SchemaFieldInfo {
  const record = isRecord(schema) ? schema : {};
  const type = schemaType(record);
  const childRequired = stringSet(record.required);
  const childProperties = isRecord(record.properties)
    ? Object.entries(record.properties).map(([childName, childSchema]) =>
        schemaFieldInfo(childName, childSchema, childRequired.has(childName), `${path}.${childName}`),
      )
    : arrayItemFields(record, path);

  return {
    path,
    name,
    type,
    description: typeof record.description === 'string' ? record.description : '',
    required,
    enumValues: enumValues(record.enum),
    children: childProperties,
  };
}

function arrayItemFields(schema: Record<string, unknown>, path: string): SchemaFieldInfo[] {
  const items = schema.items;
  if (!isRecord(items) || !isRecord(items.properties)) return [];
  const required = stringSet(items.required);
  return Object.entries(items.properties).map(([name, value]) =>
    schemaFieldInfo(name, value, required.has(name), `${path}[].${name}`),
  );
}

function schemaType(schema: Record<string, unknown>): string {
  if (typeof schema.type === 'string') {
    if (schema.type === 'array' && isRecord(schema.items)) return `array<${schemaType(schema.items)}>`;
    return schema.type;
  }
  if (Array.isArray(schema.type)) return schema.type.filter((item) => typeof item === 'string').join(' | ') || 'value';
  if (Array.isArray(schema.enum)) return 'enum';
  if (Array.isArray(schema.anyOf)) return 'anyOf';
  if (Array.isArray(schema.oneOf)) return 'oneOf';
  return 'value';
}

function stringSet(value: unknown): Set<string> {
  if (!Array.isArray(value)) return new Set();
  return new Set(value.filter((item): item is string => typeof item === 'string'));
}

function enumValues(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.map((item) => String(item)).slice(0, 12);
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
