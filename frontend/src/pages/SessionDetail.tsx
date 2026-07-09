import { useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ChevronRight, GitBranch } from 'lucide-react';
import { Area, AreaChart, CartesianGrid, ReferenceLine, XAxis, YAxis } from 'recharts';
import { svg as claudeCodeSvg } from 'thesvg/claude-code';
import { svg as codexOpenAISvg } from 'thesvg/codex-openai';
import { api } from '../api/client';
import type { AgentNode, CallEntry, CallTrace } from '../api/types';
import { ContextSunburst, prodLabel, srcColor, srcLabel, type ContextSelection } from '../components/ContextSunburst';
import { ESTIMATE_HINT, InfoHint } from '../components/InfoHint';
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
  const distribution = ctx.data?.distribution;
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
  const sessionTitle = data?.title || ctx.data?.title || '';
  const traceKey = useMemo(() => mainPromptEntries.map((entry) => entry.id).join(','), [mainPromptEntries]);
  const promptTraces = useFetch<CallTrace[]>(
    () => Promise.all(mainPromptEntries.map((entry) => api.trace(entry.id))),
    [traceKey],
    {
      enabled: mainPromptEntries.length > 0,
    },
  );
  const prompt = useMemo(
    () => (promptTraces.data ? parsePromptReconstruction(promptTraces.data) : null),
    [promptTraces.data],
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

          {mainPromptEntries.length > 0 ? (
            <PromptReconstructionCard
              prompt={prompt}
              loading={promptTraces.initialLoading || !prompt}
              error={promptTraces.error}
              onRetry={promptTraces.refetch}
            />
          ) : null}

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

          {distribution && distribution.sources.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel} style={{ marginBottom: 12, display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                Context distribution
                <InfoHint
                  text={`${ESTIMATE_HINT} This session chart sums every decomposed request window; repeated context is counted once per request.`}
                  content={
                    <>
                      <span>
                        This session chart sums every decomposed request window, so repeated context is counted once per request.
                      </span>
                      <span style={{ display: 'block', marginTop: 8 }}>{ESTIMATE_HINT}</span>
                      {latestCompositionCallId ? (
                        <span style={{ display: 'block', marginTop: 8 }}>
                          <Link to={`/calls/${latestCompositionCallId}`}>Open latest request</Link> for the single-request window.
                        </span>
                      ) : null}
                    </>
                  }
                />
              </div>
              <ContextSunburst data={distribution} active={ctxSelection} onSelect={setCtxSelection} />
              <ContextBlockDrilldown selection={ctxSelection} blocks={prompt?.blocks ?? []} />
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
                      <th>Wire</th>
                      <th>Model</th>
                      <th className="num">Tokens</th>
                      <th className="num">Cost</th>
                      <th className="num">Duration</th>
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
                        <td className="mono">{e.wire || '—'}</td>
                        <td className="mono">{e.model || '—'}</td>
                        <td className="num">{int(e.input_tokens + e.output_tokens)}</td>
                        <td className="num">{money(e.cost)}</td>
                        <td className="num">{duration(e.latency_ms / 1000)}</td>
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

function PromptReconstructionCard({
  prompt,
  loading,
  error,
  onRetry,
}: {
  prompt: PromptReconstruction | null;
  loading: boolean;
  error?: string | null;
  onRetry: () => void;
}) {
  if (error) return <ErrorBanner message={error} onRetry={onRetry} />;

  if (loading || !prompt) {
    return (
      <div className={styles.promptGrid}>
        <Skeleton height={180} />
        <Skeleton height={180} />
        <Skeleton height={220} />
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
              <pre key={i} id={systemBlockDomId(i)} className={styles.promptTextBlock}>{block}</pre>
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
              <ToolCard key={`${tool.name}-${i}`} tool={tool} index={i} />
            ))
          ) : (
            <div className={styles.promptEmpty}>No tools in this request.</div>
          )}
        </div>
      </details>

      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            Messages
          </span>
          <span className="chip chip-mono">{prompt.messages.length}</span>
        </summary>
        {prompt.messages.length > 0 ? (
          <div className={styles.messageTimeline}>
            {prompt.messages.map((message, i) => (
              <MessageCard key={`${message.role}-${i}`} message={message} index={i} />
            ))}
          </div>
        ) : (
          <div className={styles.promptEmpty}>No messages in these requests.</div>
        )}
      </details>
    </div>
  );
}

function ContextBlockDrilldown({ selection, blocks }: { selection: ContextSelection | null; blocks: PromptBlock[] }) {
  if (!selection) {
    return (
      <div className={styles.ctxBlockPanel}>
        <div className={styles.ctxBlockEmpty}>Select a context slice to inspect its prompt blocks.</div>
      </div>
    );
  }

  const matches = blocks.filter(
    (block) => block.source === selection.source && (!selection.producer || block.producer === selection.producer),
  );
  const title = selection.producer
    ? `${srcLabel(selection.source)} / ${prodLabel(selection.producer)}`
    : srcLabel(selection.source);

  return (
    <div className={styles.ctxBlockPanel}>
      <div className={styles.ctxBlockHead}>
        <span>{title}</span>
        <span className="chip chip-mono">{matches.length}</span>
      </div>
      {matches.length > 0 ? (
        <div className={styles.ctxBlockList}>
          {matches.slice(0, 24).map((block) => (
            <button key={block.id} type="button" className={styles.ctxBlockItem} onClick={() => jumpToPromptBlock(block.id)}>
              <span className={styles.ctxBlockItemTop}>
                <span className={styles.ctxBlockTitle}>{block.title}</span>
                <span className={styles.ctxBlockDetail}>{block.detail}</span>
              </span>
              {block.snippet ? <span className={styles.ctxBlockSnippet}>{snippet(block.snippet)}</span> : null}
            </button>
          ))}
        </div>
      ) : (
        <div className={styles.ctxBlockEmpty}>
          No reconstructed prompt blocks for this slice. The distribution can include repeated request windows or captured bodies that are not visible in the merged message list.
        </div>
      )}
    </div>
  );
}

function MessageCard({ message, index }: { message: PromptMessage; index: number }) {
  return (
    <div className={styles.messageCard}>
      <div className={styles.messageRole}>{message.role}</div>
      <div className={styles.messageContent}>
        {message.parts.map((part, i) => (
          <MessagePartView key={i} part={part} domId={messagePartDomId(index, [i])} path={[i]} messageIndex={index} />
        ))}
      </div>
    </div>
  );
}

function MessagePartView({
  part,
  domId,
  messageIndex,
  path,
}: {
  part: MessagePart;
  domId: string;
  messageIndex: number;
  path: number[];
}) {
  if (part.kind === 'text') {
    return (
      <div id={domId} className={`${styles.messagePartBlock} ${isSingleLineText(part.text) ? styles.messagePartSingleLine : ''}`}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy text block" className={styles.messagePartCopy} />
        <pre className={styles.messageText}>{part.text}</pre>
      </div>
    );
  }
  if (part.kind === 'tool_use') {
    return (
      <div id={part.id ? toolPartDomId('use', part.id) : domId} className={styles.messageToolBlock}>
        <div className={styles.messageToolHead}>
          <span className={styles.messageToolTitle}>
            <span>Tool Use</span>
            {part.name ? <span className={styles.messageToolName}>{part.name}</span> : null}
          </span>
          {part.id ? (
            <button
              type="button"
              className={styles.messageToolId}
              onClick={() => jumpToToolPart('result', part.id)}
              title="Jump to tool result"
            >
              {part.id}
            </button>
          ) : null}
        </div>
        {part.input !== undefined ? (
          <div className={styles.messageToolContent}>
            <div className={styles.messagePartBlock}>
              <CopyButton value={prettyJson(part.input)} ariaLabel="Copy tool input" className={styles.messagePartCopy} />
              <pre className={styles.messageJson}>{prettyJson(part.input)}</pre>
            </div>
          </div>
        ) : null}
      </div>
    );
  }
  if (part.kind === 'tool_result') {
    return (
      <div id={part.toolUseId ? toolPartDomId('result', part.toolUseId) : domId} className={styles.messageToolBlock}>
        <div className={styles.messageToolHead}>
          <span className={styles.messageToolTitle}>
            <span>Tool Result</span>
          </span>
          {part.toolUseId ? (
            <button
              type="button"
              className={styles.messageToolId}
              onClick={() => jumpToToolPart('use', part.toolUseId)}
              title="Jump to tool use"
            >
              {part.toolUseId}
            </button>
          ) : null}
        </div>
        {part.parts.length > 0 ? (
          <div className={styles.messageToolContent}>
            {part.parts.map((child, i) => (
              <MessagePartView
                key={i}
                part={child}
                domId={messagePartDomId(messageIndex, [...path, i])}
                messageIndex={messageIndex}
                path={[...path, i]}
              />
            ))}
          </div>
        ) : null}
      </div>
    );
  }
  if (part.kind === 'image') {
    return (
      <figure id={domId} className={styles.messageImageBlock}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy image source" className={styles.messagePartCopy} />
        <img className={styles.messageImage} src={part.src} alt={part.label} loading="lazy" />
        <figcaption className={styles.messageImageCaption}>{part.label}</figcaption>
      </figure>
    );
  }
  if (part.kind === 'image_url') {
    return (
      <figure id={domId} className={styles.messageImageBlock}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy image URL" className={styles.messagePartCopy} />
        <img className={styles.messageImage} src={part.url} alt={part.label} loading="lazy" />
        <figcaption className={styles.messageImageCaption}>{part.label}</figcaption>
      </figure>
    );
  }
  if (part.kind === 'empty') {
    return (
      <div id={domId} className={styles.messageEmptyPart}>
        <span>{part.label}</span>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy block" className={styles.messagePartCopy} />
      </div>
    );
  }
  return (
    <div id={domId} className={`${styles.messagePartBlock} ${styles.messagePartSingleLine}`}>
      <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy raw block" className={styles.messagePartCopy} />
      <details className={styles.messageRawPart}>
        <summary>{part.label}</summary>
        <pre className={styles.messageJson}>{prettyJson(part.raw)}</pre>
      </details>
    </div>
  );
}

function ToolCard({ tool, index }: { tool: ToolInfo; index: number }) {
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
    <details ref={ref} id={toolBlockDomId(index)} className={styles.toolCard} onToggle={handleToggle}>
      <summary className={styles.toolSummary}>
        <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
        <span className={styles.toolMeta}>{toolMeta(tool)}</span>
      </summary>
      <div className={`${styles.toolDetail} ${align === 'right' ? styles.toolDetailRight : styles.toolDetailLeft}`}>
        {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
        {tool.type === 'namespace' ? (
          <div className={styles.namespaceToolList}>
            {tool.tools.length > 0 ? (
              tool.tools.map((child, i) => (
                <NestedToolCard key={`${child.name}-${i}`} tool={child} />
              ))
            ) : (
              <div className={styles.schemaEmpty}>No tools in this namespace.</div>
            )}
          </div>
        ) : (
          <SchemaView schema={tool.schema} />
        )}
      </div>
    </details>
  );
}

function NestedToolCard({ tool }: { tool: ToolInfo }) {
  if (tool.type === 'namespace') {
    return (
      <details className={styles.namespaceNestedCard}>
        <summary className={styles.namespaceNestedSummary}>
          <span className={styles.toolName}>{tool.name || '(unnamed namespace)'}</span>
          <span className={styles.toolMeta}>{toolMeta(tool)}</span>
        </summary>
        {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
        <div className={styles.namespaceToolList}>
          {tool.tools.length > 0 ? (
            tool.tools.map((child, i) => (
              <NestedToolCard key={`${child.name}-${i}`} tool={child} />
            ))
          ) : (
            <div className={styles.schemaEmpty}>No tools in this namespace.</div>
          )}
        </div>
      </details>
    );
  }

  return (
    <details className={styles.namespaceNestedCard}>
      <summary className={styles.namespaceNestedSummary}>
        <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
        <span className={styles.toolMeta}>{toolMeta(tool)}</span>
      </summary>
      {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
      <SchemaView schema={tool.schema} />
    </details>
  );
}

function toolMeta(tool: ToolInfo): string {
  if (tool.type === 'namespace') return `${tool.tools.length} ${tool.tools.length === 1 ? 'tool' : 'tools'}`;
  return `${tool.propertyCount} ${tool.propertyCount === 1 ? 'input' : 'inputs'}`;
}

interface PromptReconstruction {
  system: string[];
  tools: ToolInfo[];
  messages: PromptMessage[];
  blocks: PromptBlock[];
}

interface ToolInfo {
  type: string;
  name: string;
  description: string;
  propertyCount: number;
  schema: unknown;
  tools: ToolInfo[];
}

interface PromptBlock {
  id: string;
  source: string;
  producer?: string;
  title: string;
  detail: string;
  snippet: string;
}

interface PromptMessage {
  role: string;
  parts: MessagePart[];
}

type MessagePart =
  | { kind: 'text'; text: string }
  | { kind: 'tool_use'; id: string; name: string; input: unknown }
  | { kind: 'tool_result'; toolUseId: string; parts: MessagePart[] }
  | { kind: 'image'; src: string; label: string }
  | { kind: 'image_url'; url: string; label: string }
  | { kind: 'empty'; label: string }
  | { kind: 'raw'; label: string; raw: unknown };

function isMainPromptEntry(entry: CallEntry): boolean {
  return entry.has_trace && entry.wire !== 'anthropic/count_tokens' && !isHaikuAuxiliary(entry);
}

function isHaikuAuxiliary(entry: CallEntry): boolean {
  return entry.wire === 'anthropic/messages' && entry.model.toLowerCase().includes('haiku');
}

function parsePromptReconstruction(traces: CallTrace[]): PromptReconstruction {
  const firstBody = parseJsonObject(traces[0]?.request.body ?? '');
  let messages: PromptMessage[] = [];

  for (let i = 0; i < traces.length; i += 1) {
    const body = parseJsonObject(traces[i].request.body);
    if (!body) continue;
    messages = mergePromptMessages(messages, promptMessages(body));
  }

  const prompt = {
    system: systemBlocks(firstBody?.system ?? firstBody?.instructions),
    tools: toolInfos(firstBody?.tools),
    messages,
  };
  return {
    ...prompt,
    blocks: promptBlocks(prompt),
  };
}

function promptBlocks(prompt: Omit<PromptReconstruction, 'blocks'>): PromptBlock[] {
  const out: PromptBlock[] = [];

  prompt.system.forEach((block, i) => {
    out.push({
      id: systemBlockDomId(i),
      source: 'system',
      title: `System block ${i + 1}`,
      detail: 'System prompt',
      snippet: snippet(block),
    });
  });

  prompt.tools.forEach((tool, i) => {
    out.push({
      id: toolBlockDomId(i),
      source: 'tool_schemas',
      producer: schemaProducer(tool.name),
      title: tool.name || `Tool schema ${i + 1}`,
      detail: 'Tool schema',
      snippet: tool.description || toolMeta(tool),
    });
  });

  const toolNames = toolUseNames(prompt.messages);
  prompt.messages.forEach((message, messageIndex) => {
    message.parts.forEach((part, partIndex) => {
      collectPartBlocks(out, message.role, part, messageIndex, [partIndex], toolNames);
    });
  });

  return out;
}

function collectPartBlocks(
  out: PromptBlock[],
  role: string,
  part: MessagePart,
  messageIndex: number,
  path: number[],
  toolNames: Map<string, string>,
) {
  const baseId = messagePartDomId(messageIndex, path);
  const baseDetail = `${role} message`;
  if (part.kind === 'tool_result') {
    const producer = toolProducer(toolNames.get(part.toolUseId) ?? '');
    out.push({
      id: part.toolUseId ? toolPartDomId('result', part.toolUseId) : baseId,
      source: 'tool_results',
      producer,
      title: part.toolUseId ? `Tool result ${part.toolUseId}` : 'Tool result',
      detail: baseDetail,
      snippet: part.parts.map(messagePartCopyText).join('\n'),
    });
    return;
  }
  if (part.kind === 'tool_use') {
    out.push({
      id: part.id ? toolPartDomId('use', part.id) : baseId,
      source: 'actions',
      title: part.name || 'Tool use',
      detail: baseDetail,
      snippet: messagePartCopyText(part),
    });
    return;
  }
  if (part.kind === 'image' || part.kind === 'image_url') {
    out.push({
      id: baseId,
      source: 'attachments',
      title: part.label,
      detail: baseDetail,
      snippet: messagePartCopyText(part),
    });
    return;
  }
  if (part.kind === 'raw' && (part.label.includes('thinking') || part.label === 'reasoning')) {
    const text = reasoningPartText(part);
    if (!text) return;
    out.push({
      id: baseId,
      source: 'reasoning',
      title: part.label,
      detail: baseDetail,
      snippet: text,
    });
    return;
  }

  const source = role === 'assistant' ? 'actions' : role === 'system' || role === 'developer' ? 'system' : 'user';
  out.push({
    id: baseId,
    source,
    title: partTitle(part),
    detail: baseDetail,
    snippet: messagePartCopyText(part),
  });
}

function toolUseNames(messages: PromptMessage[]): Map<string, string> {
  const out = new Map<string, string>();
  const visit = (part: MessagePart) => {
    if (part.kind === 'tool_use' && part.id) out.set(part.id, part.name);
    if (part.kind === 'tool_result') part.parts.forEach(visit);
  };
  messages.forEach((message) => message.parts.forEach(visit));
  return out;
}

function partTitle(part: MessagePart): string {
  if (part.kind === 'text') return 'Text block';
  if (part.kind === 'empty') return part.label;
  if (part.kind === 'raw') return part.label;
  return messagePartCopyText(part).split('\n')[0] || 'Message block';
}

function reasoningPartText(part: Extract<MessagePart, { kind: 'raw' }>): string {
  if (!isRecord(part.raw)) return '';
  if (typeof part.raw.thinking === 'string') return part.raw.thinking;
  if (Array.isArray(part.raw.summary)) {
    return part.raw.summary
      .map((item) => (isRecord(item) && typeof item.text === 'string' ? item.text : ''))
      .filter(Boolean)
      .join('\n');
  }
  return '';
}

function snippet(text: string): string {
  return text.replace(/\s+/g, ' ').trim().slice(0, 220);
}

function promptMessages(body: Record<string, unknown>): PromptMessage[] {
  const source = Array.isArray(body.messages) ? body.messages : body.input;
  if (typeof source === 'string') return [{ role: 'user', parts: [{ kind: 'text', text: source }] }];
  if (!Array.isArray(source)) return [];
  return source.map(promptMessage).filter((message) => message.parts.length > 0);
}

function promptMessage(raw: unknown): PromptMessage {
  if (typeof raw === 'string') return { role: 'user', parts: [{ kind: 'text', text: raw }] };
  if (!isRecord(raw)) return { role: 'unknown', parts: [{ kind: 'raw', label: 'value', raw: stripCacheControl(raw) }] };

  const type = typeof raw.type === 'string' ? raw.type : '';
  if (type === 'function_call') {
    return {
      role: 'assistant',
      parts: [functionCallPart(raw)],
    };
  }
  if (type === 'function_call_output') {
    return {
      role: 'tool',
      parts: [functionCallOutputPart(raw)],
    };
  }

  const role =
    typeof raw.role === 'string'
      ? raw.role
      : type === 'message'
        ? 'message'
        : type || 'unknown';
  if (role === 'tool' && typeof raw.tool_call_id === 'string') {
    return {
      role,
      parts: [{ kind: 'tool_result', toolUseId: raw.tool_call_id, parts: messageParts(raw.content) }],
    };
  }
  const parts = messageParts(raw.content);
  if (Array.isArray(raw.tool_calls)) {
    parts.push(...raw.tool_calls.map(functionCallPart));
  }
  if (parts.length === 0) parts.push({ kind: 'raw', label: type || role, raw: stripCacheControl(raw) });
  return { role, parts };
}

function messageParts(content: unknown): MessagePart[] {
  if (typeof content === 'string') return [{ kind: 'text', text: content }];
  if (Array.isArray(content)) return content.flatMap(messagePart);
  if (content == null) return [];
  return [messagePart(content)].flat();
}

function messagePart(raw: unknown): MessagePart | MessagePart[] {
  if (typeof raw === 'string') return { kind: 'text', text: raw };
  if (!isRecord(raw)) return { kind: 'raw', label: 'value', raw: stripCacheControl(raw) };

  const type = typeof raw.type === 'string' ? raw.type : '';
  if (type === 'tool_use') {
    return {
      kind: 'tool_use',
      id: typeof raw.id === 'string' ? raw.id : '',
      name: typeof raw.name === 'string' ? raw.name : '',
      input: stripCacheControl(raw.input ?? {}),
    };
  }
  if (type === 'tool_result') {
    return {
      kind: 'tool_result',
      toolUseId: typeof raw.tool_use_id === 'string' ? raw.tool_use_id : '',
      parts: messageParts(raw.content),
    };
  }
  if (type === 'image') {
    const image = imagePart(raw);
    if (image) return image;
  }
  if (type === 'input_image') {
    const image = inputImagePart(raw);
    if (image) return image;
  }
  if (type === 'function_call') return functionCallPart(raw);
  if (type === 'function_call_output') return functionCallOutputPart(raw);

  const text = textField(raw);
  if (text !== null) return { kind: 'text', text };
  return { kind: 'raw', label: type || 'part', raw: stripCacheControl(raw) };
}

function functionCallPart(raw: unknown): MessagePart {
  const record = isRecord(raw) ? raw : {};
  const fn = isRecord(record.function) ? record.function : {};
  const name =
    typeof record.name === 'string'
      ? record.name
      : typeof fn.name === 'string'
        ? fn.name
        : '';
  const id =
    typeof record.id === 'string'
      ? record.id
      : typeof record.call_id === 'string'
        ? record.call_id
        : '';
  const input = record.arguments ?? fn.arguments ?? record.input ?? {};
  return {
    kind: 'tool_use',
    id,
    name,
    input: parseJsonMaybe(input),
  };
}

function functionCallOutputPart(raw: unknown): MessagePart {
  const record = isRecord(raw) ? raw : {};
  return {
    kind: 'tool_result',
    toolUseId: typeof record.call_id === 'string' ? record.call_id : '',
    parts: messageParts(record.output),
  };
}

function imagePart(record: Record<string, unknown>): MessagePart | null {
  const source = isRecord(record.source) ? record.source : null;
  if (!source) return null;
  if (typeof source.url === 'string') {
    return { kind: 'image_url', url: source.url, label: 'Image' };
  }
  if (source.type === 'base64' && typeof source.data === 'string') {
    const mediaType = typeof source.media_type === 'string' ? source.media_type : 'image/jpeg';
    return { kind: 'image', src: `data:${mediaType};base64,${source.data}`, label: mediaType };
  }
  return null;
}

function inputImagePart(record: Record<string, unknown>): MessagePart | null {
  if (typeof record.image_url === 'string') {
    return { kind: 'image_url', url: record.image_url, label: 'Image' };
  }
  if (isRecord(record.image_url) && typeof record.image_url.url === 'string') {
    return { kind: 'image_url', url: record.image_url.url, label: 'Image' };
  }
  return null;
}

function textField(record: Record<string, unknown>): string | null {
  if (typeof record.text === 'string') return record.text;
  if (typeof record.output_text === 'string') return record.output_text;
  if (typeof record.input_text === 'string') return record.input_text;
  return null;
}

function parseJsonMaybe(value: unknown): unknown {
  if (typeof value !== 'string') return stripCacheControl(value);
  try {
    return stripCacheControl(JSON.parse(value));
  } catch {
    return value;
  }
}

function mergePromptMessages(existing: PromptMessage[], next: PromptMessage[]): PromptMessage[] {
  if (next.length === 0) return existing;
  let overlap = Math.min(existing.length, next.length);
  while (overlap > 0) {
    const existingStart = existing.length - overlap;
    let matches = true;
    for (let i = 0; i < overlap; i += 1) {
      if (!sameMessage(existing[existingStart + i], next[i])) {
        matches = false;
        break;
      }
    }
    if (matches) break;
    overlap -= 1;
  }
  return existing.concat(next.slice(overlap));
}

function sameMessage(a: PromptMessage, b: PromptMessage): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function systemBlockDomId(index: number): string {
  return `prompt-system-${index}`;
}

function toolBlockDomId(index: number): string {
  return `prompt-tool-${index}`;
}

function messagePartDomId(messageIndex: number, path: number[]): string {
  return `prompt-message-${messageIndex}-part-${path.join('-')}`;
}

function toolPartDomId(kind: 'use' | 'result', id: string): string {
  return `tool-${kind}-${id.replace(/[^A-Za-z0-9_-]/g, '_')}`;
}

function jumpToPromptBlock(id: string): void {
  const el = document.getElementById(id);
  if (!el) return;
  revealDetails(el);
  highlightElement(el);
}

function jumpToToolPart(kind: 'use' | 'result', id: string): void {
  const el = document.getElementById(toolPartDomId(kind, id));
  if (!el) return;
  revealDetails(el);
  highlightElement(el);
}

function revealDetails(el: HTMLElement): void {
  let parent = el.parentElement;
  while (parent) {
    if (parent instanceof HTMLDetailsElement) parent.open = true;
    parent = parent.parentElement;
  }
}

function highlightElement(el: HTMLElement): void {
  el.scrollIntoView({ behavior: 'auto', block: 'start' });
  el.classList.remove(styles.messageJumpHighlight);
  window.requestAnimationFrame(() => {
    el.classList.add(styles.messageJumpHighlight);
    window.setTimeout(() => el.classList.remove(styles.messageJumpHighlight), 1800);
  });
}

function messagePartCopyText(part: MessagePart): string {
  if (part.kind === 'text') return part.text;
  if (part.kind === 'tool_use') {
    const name = part.name ? ` ${part.name}` : '';
    const id = part.id ? ` (${part.id})` : '';
    return `Tool Use${name}${id}\n${prettyJson(part.input)}`;
  }
  if (part.kind === 'tool_result') {
    const id = part.toolUseId ? ` ${part.toolUseId}` : '';
    return [`Tool Result${id}`, ...part.parts.map(messagePartCopyText)].join('\n');
  }
  if (part.kind === 'image') return `Image: ${part.label}\n${part.src}`;
  if (part.kind === 'image_url') return `Image: ${part.label}\n${part.url}`;
  if (part.kind === 'empty') return part.label;
  return `${part.label}\n${prettyJson(part.raw)}`;
}

function isSingleLineText(text: string): boolean {
  return !text.includes('\n') && text.length <= 160;
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
  return tools.map(toolInfo);
}

function toolInfo(tool: unknown): ToolInfo {
  const record = isRecord(tool) ? tool : {};
  const fn = isRecord(record.function) ? record.function : {};
  const type = typeof record.type === 'string' ? record.type : 'function';
  const schema = record.input_schema ?? record.parameters ?? fn.parameters;
  const nestedTools = Array.isArray(record.tools) ? record.tools.map(toolInfo) : [];
  return {
    type,
    name:
      typeof record.name === 'string'
        ? record.name
        : typeof fn.name === 'string'
          ? fn.name
          : type,
    description:
      typeof record.description === 'string'
        ? record.description
        : typeof fn.description === 'string'
          ? fn.description
          : '',
    propertyCount: schemaPropertyCount(schema),
    schema: stripCacheControl(schema ?? {}),
    tools: nestedTools,
  };
}

function toolProducer(name: string): string {
  switch (name) {
    case 'Read':
      return 'read';
    case 'Bash':
      return 'bash';
    case 'Grep':
      return 'grep';
    case 'Glob':
      return 'glob';
    case 'Task':
      return 'task';
    case 'WebFetch':
    case 'WebSearch':
      return 'web';
    default:
      break;
  }
  const server = mcpServer(name);
  if (server) return `mcp:${server}`;
  return name ? name.toLowerCase() : 'unknown';
}

function schemaProducer(name: string): string {
  const server = mcpServer(name);
  return server ? `mcp:${server}` : 'builtin';
}

function mcpServer(name: string): string {
  if (!name.startsWith('mcp__')) return '';
  const parts = name.split('__');
  return parts[1] || 'mcp';
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

function compactTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`;
  return `${Math.round(n)}`;
}
