import { Cell, Pie, PieChart } from 'recharts';
import type { SourceSlice } from '../api/types';
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from './ui/chart';
import styles from './ContextSunburst.module.css';

const CHART_CLS = 'aspect-auto h-full w-full';

/** Source-bucket labels + colors, shared by the sunburst and its legend. */
export const SRC_META: Record<string, { label: string; color: string }> = {
  tool_results: { label: 'Tool results', color: 'var(--chart-1)' },
  tool_schemas: { label: 'Tool schemas', color: 'var(--chart-2)' },
  system: { label: 'System & instructions', color: 'var(--chart-6)' },
  reasoning: { label: 'Assistant reasoning', color: 'var(--chart-4)' },
  actions: { label: 'Assistant actions', color: 'var(--chart-5)' },
  user: { label: 'User turns', color: 'var(--chart-3)' },
  attachments: { label: 'Attachments', color: 'var(--chart-7)' },
  unattributed: { label: 'Unattributed', color: 'var(--text-muted)' },
};

const PROD_PALETTE = [
  'var(--chart-1)', 'var(--chart-2)', 'var(--chart-6)', 'var(--chart-4)',
  'var(--chart-3)', 'var(--chart-7)', 'var(--chart-5)',
];

export function srcColor(key: string): string {
  return SRC_META[key]?.color ?? 'var(--text-muted)';
}

export function srcLabel(key: string): string {
  return SRC_META[key]?.label ?? key;
}

export function prodLabel(key: string): string {
  if (key.startsWith('mcp:')) return key.slice(4);
  const m: Record<string, string> = {
    read: 'Read', bash: 'Bash', grep: 'Grep', glob: 'Glob', task: 'Task',
    skill: 'Skill', builtin: 'built-in', base: 'base prompt',
    claude_md: 'CLAUDE.md', memory: 'memory', web: 'Web', unknown: 'unknown',
  };
  return m[key] ?? key;
}

/** Compact token count for the center label, e.g. 12.3k, 4.5M. */
function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return `${Math.round(n)}`;
}

interface SunburstData {
  avg_total: number;
  sources: SourceSlice[];
}

/**
 * Two-level composition ring: inner = source buckets, outer = producer
 * drill-down. Childless buckets extend their parent as one faded outer segment
 * so the ring stays whole. Both rings sum to the same total, so they align.
 */
export function ContextSunburst({ data, centerLabel = 'avg window' }: { data: SunburstData; centerLabel?: string }) {
  const sources = data.sources;
  const total = sources.reduce((a, s) => a + s.tokens, 0) || 1;

  // Stable color per producer child, reused by both the ring and the legend.
  const kidColor = new Map<string, string>();
  let pi = 0;
  for (const s of sources) {
    for (const c of s.children ?? []) kidColor.set(`${s.key}/${c.key}`, PROD_PALETTE[pi++ % PROD_PALETTE.length]);
  }

  const inner = sources.map((s) => ({ name: srcLabel(s.key), tokens: s.tokens, color: srcColor(s.key) }));

  const outer: { name: string; tokens: number; color: string; faded?: boolean }[] = [];
  for (const s of sources) {
    if (s.children?.length) {
      for (const c of s.children)
        outer.push({ name: prodLabel(c.key), tokens: c.tokens, color: kidColor.get(`${s.key}/${c.key}`)! });
    } else {
      outer.push({ name: srcLabel(s.key), tokens: s.tokens, color: srcColor(s.key), faded: true });
    }
  }

  const pct = (n: number) => `${Math.round((n / total) * 100)}%`;

  return (
    <div className={styles.sunWrap}>
      <div className={styles.sunChart}>
        <ChartContainer config={{}} className={CHART_CLS}>
          <PieChart>
            <ChartTooltip content={<ChartTooltipContent nameKey="name" />} />
            <Pie data={inner} dataKey="tokens" nameKey="name" innerRadius="42%" outerRadius="62%" strokeWidth={1.5} stroke="var(--surface)" isAnimationActive={false}>
              {inner.map((d, i) => (
                <Cell key={i} fill={d.color} />
              ))}
            </Pie>
            <Pie data={outer} dataKey="tokens" nameKey="name" innerRadius="64%" outerRadius="84%" strokeWidth={1.5} stroke="var(--surface)" isAnimationActive={false}>
              {outer.map((d, i) => (
                <Cell key={i} fill={d.color} fillOpacity={d.faded ? 0.28 : 1} />
              ))}
            </Pie>
          </PieChart>
        </ChartContainer>
        <div className={styles.sunCenter}>
          <div className={styles.sunCenterVal}>{compact(data.avg_total)}</div>
          <div className={styles.sunCenterLbl}>{centerLabel}</div>
        </div>
      </div>
      <div className={styles.nestLegend}>
        {sources.map((s) => (
          <div key={s.key}>
            <div className={styles.nlParent}>
              <span className={styles.nlSw} style={{ background: srcColor(s.key) }} />
              {srcLabel(s.key)}
              <b className={styles.nlPct}>{pct(s.tokens)}</b>
            </div>
            {s.children?.length ? (
              <div className={styles.nlKids}>
                {s.children.map((c) => (
                  <span key={c.key}>
                    <span className={styles.nlDot} style={{ background: kidColor.get(`${s.key}/${c.key}`) }} />
                    {prodLabel(c.key)} <span className="mono">{pct(c.tokens)}</span>
                  </span>
                ))}
              </div>
            ) : null}
          </div>
        ))}
      </div>
    </div>
  );
}
