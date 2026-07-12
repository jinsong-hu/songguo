import type { ReactNode } from 'react';
import { Cell, Pie, PieChart } from 'recharts';
import type { SourceSlice } from '../api/types';
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from './ui/chart';
import { InfoHint } from './InfoHint';
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
  'var(--producer-1)', 'var(--producer-2)', 'var(--producer-3)', 'var(--producer-4)',
  'var(--producer-5)', 'var(--producer-6)', 'var(--producer-7)', 'var(--producer-8)',
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

export interface ContextSelection {
  source: string;
  producer?: string;
}

const MAIN_SIZE = 240;
const MAIN_CENTER = MAIN_SIZE / 2;
const MAIN_OUTER_R = MAIN_CENTER * 0.78;
const CONNECTOR_W = 54;
const BREAKOUT_LIMIT = 4;

type LayoutMode = 'one' | 'two' | 'four';

function sideRows(n: number): number[] {
  return Array.from({ length: n }, (_, i) => (MAIN_SIZE * (i + 0.5)) / n);
}

function chooseBreakouts(sources: SourceSlice[], total: number): { mode: LayoutMode; breakouts: SourceSlice[] } {
  // Rank every source bucket by size — a breakout is drawn for the largest
  // buckets whether or not they carry a producer drill-down. Copy before
  // sorting so we don't reorder the caller's array (the legend keeps its order).
  const candidates = sources
    .slice()
    .sort((a, b) => b.tokens - a.tokens);
  const share = (i: number) => (candidates[i]?.tokens ?? 0) / total;
  if (candidates.length <= 1 || share(1) < 0.1) {
    return { mode: 'one', breakouts: candidates.slice(0, 1) };
  }
  if (candidates.length <= 2 || share(2) < 0.1) {
    return { mode: 'two', breakouts: candidates.slice(0, 2) };
  }
  return { mode: 'four', breakouts: candidates.slice(0, BREAKOUT_LIMIT) };
}

function sectorMidAngles(sources: SourceSlice[], total: number): Map<string, number> {
  const out = new Map<string, number>();
  let cursor = 90;
  for (const s of sources) {
    const sweep = (s.tokens / total) * 360;
    out.set(s.key, cursor - sweep / 2);
    cursor -= sweep;
  }
  return out;
}

function naturalSide(s: SourceSlice, angles: Map<string, number>): 'left' | 'right' {
  const angle = ((angles.get(s.key) ?? 0) * Math.PI) / 180;
  return Math.cos(angle) >= 0 ? 'right' : 'left';
}

function connectorPath(
  source: SourceSlice,
  side: 'left' | 'right',
  targetY: number,
  angles: Map<string, number>,
): string {
  const angle = ((angles.get(source.key) ?? 0) * Math.PI) / 180;
  const cos = Math.cos(angle);
  const sin = Math.sin(angle);
  const startX = MAIN_CENTER + cos * MAIN_OUTER_R;
  const startY = MAIN_CENTER - sin * MAIN_OUTER_R;
  const radialX = MAIN_CENTER + cos * (MAIN_OUTER_R + 22);
  const radialY = MAIN_CENTER - sin * (MAIN_OUTER_R + 22);
  const endX = side === 'left' ? -CONNECTOR_W : MAIN_SIZE + CONNECTOR_W;
  const approachX = endX + (side === 'left' ? 22 : -22);
  return `M ${startX} ${startY} C ${radialX} ${radialY}, ${approachX} ${targetY}, ${endX} ${targetY}`;
}

function pushBalanced(cols: { left: SourceSlice[]; right: SourceSlice[] }, side: 'left' | 'right', s: SourceSlice) {
  const preferred = cols[side];
  const other = side === 'left' ? cols.right : cols.left;
  if (preferred.length < 2) preferred.push(s);
  else other.push(s);
}

function breakoutColumns(
  mode: LayoutMode,
  breakouts: SourceSlice[],
  angles: Map<string, number>,
): { left: SourceSlice[]; right: SourceSlice[] } {
  const cols = { left: [] as SourceSlice[], right: [] as SourceSlice[] };
  if (mode === 'one') {
    const s = breakouts[0];
    if (s) cols[naturalSide(s, angles)].push(s);
    return cols;
  }
  if (mode === 'two') {
    for (const s of breakouts) {
      const side = naturalSide(s, angles);
      if (cols[side].length === 0) cols[side].push(s);
      else cols[side === 'left' ? 'right' : 'left'].push(s);
    }
    return cols;
  }
  for (const s of breakouts) pushBalanced(cols, naturalSide(s, angles), s);
  return cols;
}

/**
 * Source-level context donut with producer drill-downs pulled out to small
 * side donuts for the largest child-bearing source buckets.
 */
export function ContextSunburst({
  data,
  centerValue,
  centerLabel = '',
  active,
  onSelect,
}: {
  data: SunburstData;
  centerValue?: number;
  centerLabel?: string;
  active?: ContextSelection | null;
  onSelect?: (selection: ContextSelection | null) => void;
}) {
  const sources = data.sources;
  const total = sources.reduce((a, s) => a + s.tokens, 0) || 1;

  // Stable color per producer child, reused by both the ring and the legend.
  const kidColor = new Map<string, string>();
  let pi = 0;
  for (const s of sources) {
    for (const c of s.children ?? []) kidColor.set(`${s.key}/${c.key}`, PROD_PALETTE[pi++ % PROD_PALETTE.length]);
  }

  const inner = sources.map((s) => ({ key: s.key, name: srcLabel(s.key), tokens: s.tokens, color: srcColor(s.key) }));
  const { mode, breakouts } = chooseBreakouts(sources, total);
  const angles = sectorMidAngles(sources, total);
  const { left, right } = breakoutColumns(mode, breakouts, angles);
  const leftRows = sideRows(left.length);
  const rightRows = sideRows(right.length);
  const modeClass = mode === 'one'
    ? left.length > 0
      ? styles.modeOneLeft
      : styles.modeOneRight
    : mode === 'two'
      ? styles.modeTwo
      : styles.modeFour;

  const pct = (n: number) => `${Math.round((n / total) * 100)}%`;
  const isActive = (source: string, producer?: string) =>
    active?.source === source && (active.producer ?? '') === (producer ?? '');
  const selectSource = (source: string) => onSelect?.(isActive(source) ? null : { source });
  const selectProducer = (source: string, producer: string) => onSelect?.(isActive(source, producer) ? null : { source, producer });

  const breakoutCard = (s: SourceSlice, side: 'left' | 'right') => {
    const children = s.children ?? [];
    const hasKids = children.length > 0;
    const childTotal = children.reduce((a, c) => a + c.tokens, 0) || 1;
    // Ring segments: producer children when the bucket has a drill-down,
    // otherwise the whole bucket as one solid ring (same chrome, no breakdown).
    const segments = hasKids
      ? children.map((c) => ({
          key: c.key,
          name: prodLabel(c.key),
          tokens: c.tokens,
          color: kidColor.get(`${s.key}/${c.key}`)!,
          selected: isActive(s.key, c.key),
          onClick: () => selectProducer(s.key, c.key),
        }))
      : [{
          key: s.key,
          name: srcLabel(s.key),
          tokens: s.tokens,
          color: srcColor(s.key),
          selected: isActive(s.key),
          onClick: () => selectSource(s.key),
        }];
    return (
      <div key={s.key} className={`${styles.breakout} ${side === 'left' ? styles.breakoutLeft : styles.breakoutRight}`}>
        <div className={styles.miniChart}>
          <ChartContainer config={{}} className={CHART_CLS}>
            <PieChart>
              <ChartTooltip content={<ChartTooltipContent nameKey="name" />} />
              <Pie
                data={segments}
                dataKey="tokens"
                nameKey="name"
                innerRadius="56%"
                outerRadius="86%"
                startAngle={90}
                endAngle={-270}
                strokeWidth={1}
                stroke="var(--surface)"
                isAnimationActive={false}
              >
                {segments.map((seg) => (
                  <Cell
                    key={seg.key}
                    fill={seg.color}
                    cursor={onSelect ? 'pointer' : undefined}
                    opacity={active && !seg.selected ? 0.5 : 1}
                    onClick={seg.onClick}
                  />
                ))}
              </Pie>
            </PieChart>
          </ChartContainer>
        </div>
        <div className={styles.breakoutText}>
          <button
            type="button"
            className={`${styles.breakoutTitle} ${styles.chartPick} ${isActive(s.key) ? styles.chartPickActive : ''}`}
            onClick={() => selectSource(s.key)}
            disabled={!onSelect}
          >
            <span className={styles.nlSw} style={{ background: srcColor(s.key) }} />
            {srcLabel(s.key)}
            <b className={styles.nlPct}>{pct(s.tokens)}</b>
          </button>
          {hasKids ? (
            <div className={styles.breakoutKids}>
              {children.slice(0, 4).map((c) => (
                <button
                  type="button"
                  key={c.key}
                  className={`${styles.kidPick} ${isActive(s.key, c.key) ? styles.chartPickActive : ''}`}
                  onClick={() => selectProducer(s.key, c.key)}
                  disabled={!onSelect}
                >
                  <span className={styles.nlDot} style={{ background: kidColor.get(`${s.key}/${c.key}`) }} />
                  {prodLabel(c.key)} <span className="mono">{Math.round((c.tokens / childTotal) * 100)}%</span>
                </button>
              ))}
            </div>
          ) : (
            <div className={styles.breakoutKids}>
              <span className={styles.breakoutNoKids}>no breakdown</span>
            </div>
          )}
        </div>
      </div>
    );
  };

  return (
    <div className={`${styles.donutWrap} ${modeClass}`}>
      {left.length > 0 ? (
        <div className={`${styles.breakoutColumn} ${styles.leftBreakouts}`}>
          {left.map((s) => breakoutCard(s, 'left'))}
        </div>
      ) : null}
      <div className={styles.chartCluster}>
        <div className={styles.sunChart}>
          <ChartContainer config={{}} className={CHART_CLS}>
            <PieChart>
              <ChartTooltip content={<ChartTooltipContent nameKey="name" />} />
              <Pie
                data={inner}
                dataKey="tokens"
                nameKey="name"
                innerRadius="54%"
                outerRadius="78%"
                startAngle={90}
                endAngle={-270}
                strokeWidth={1.5}
                stroke="var(--surface)"
                isAnimationActive={false}
              >
                {inner.map((d) => (
                  <Cell
                    key={d.key}
                    fill={d.color}
                    cursor={onSelect ? 'pointer' : undefined}
                    opacity={active && active.source !== d.key ? 0.45 : 1}
                    onClick={() => selectSource(d.key)}
                  />
                ))}
              </Pie>
            </PieChart>
          </ChartContainer>
          <div className={styles.sunCenter}>
            <div className={styles.sunCenterVal}>{compact(centerValue ?? total)}</div>
            {centerLabel ? <div className={styles.sunCenterLbl}>{centerLabel}</div> : null}
          </div>
        </div>
        {breakouts.length > 0 ? (
          <svg className={styles.connectorLayer} viewBox={`${-CONNECTOR_W} 0 ${MAIN_SIZE + CONNECTOR_W * 2} ${MAIN_SIZE}`} aria-hidden="true">
            {left.map((s, i) => {
              const y = leftRows[i];
              return (
                <path
                  key={s.key}
                  d={connectorPath(s, 'left', y, angles)}
                  stroke={srcColor(s.key)}
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  fill="none"
                  opacity="0.72"
                />
              );
            })}
            {right.map((s, i) => {
              const y = rightRows[i];
              return (
                <path
                  key={s.key}
                  d={connectorPath(s, 'right', y, angles)}
                  stroke={srcColor(s.key)}
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  fill="none"
                  opacity="0.72"
                />
              );
            })}
          </svg>
        ) : null}
      </div>
      {right.length > 0 ? (
        <div className={`${styles.breakoutColumn} ${styles.rightBreakouts}`}>
          {right.map((s) => breakoutCard(s, 'right'))}
        </div>
      ) : null}
      <div className={styles.sourceLegend}>
        {sources.map((s) => (
          <div key={s.key}>
            <button
              type="button"
              className={`${styles.nlParent} ${styles.chartPick} ${isActive(s.key) ? styles.chartPickActive : ''}`}
              onClick={() => selectSource(s.key)}
              disabled={!onSelect}
            >
              <span className={styles.nlSw} style={{ background: srcColor(s.key) }} />
              {srcLabel(s.key)}
              <b className={styles.nlPct}>{pct(s.tokens)}</b>
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * Shared card chrome for the context-distribution view — the "Context
 * distribution" heading + info hint + a padded card. Both the Overview and
 * Session-detail pages wrap their `ContextSunburst` (and any drill-down) in
 * this, so the two read identically. Pass `info` to override the default
 * `InfoHint`; `children` holds the donut and page-specific extras.
 */
export function ContextDistributionCard({
  info,
  children,
}: {
  info?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className={`card ${styles.ctxCard}`}>
      <div className={styles.ctxHeading}>
        Context distribution
        {info ?? <InfoHint />}
      </div>
      {children}
    </div>
  );
}
