import { useMemo, useState, type CSSProperties } from 'react';
import type { CallEntry } from '../api/types';
import { duration, int, percent } from '../lib/format';
import styles from './SessionTimeline.module.css';

// A call's kind on the timeline. Derived from the stored entrypoint (the classifier's
// verdict) plus tool_calls and status — never from the request body.
type CatKey = 'core_tool' | 'core_text' | 'monitor' | 'count_tokens' | 'utility' | 'error' | 'pending';

const CATS: Record<CatKey, { label: string; color: string; hatch?: boolean }> = {
  core_tool: { label: 'Tool turn', color: 'var(--chart-1)' },
  core_text: { label: 'Text turn', color: 'var(--chart-4)' },
  monitor: { label: 'Monitor', color: 'var(--chart-3)' },
  count_tokens: { label: 'Count-tokens', color: 'var(--chart-5)' },
  utility: { label: 'Utility', color: 'var(--chart-2)' },
  error: { label: 'Failed / aborted', color: 'var(--danger)', hatch: true },
  pending: { label: 'In flight', color: 'var(--text-muted)' },
};

function categorize(e: CallEntry): CatKey {
  if (e.pending) return 'pending';
  if (e.status <= 0 || e.status >= 400) return 'error';
  switch (e.entrypoint) {
    case 'monitor':
      return 'monitor';
    case 'count_tokens':
      return 'count_tokens';
    case 'utility':
      return 'utility';
    default:
      return e.tool_calls > 0 ? 'core_tool' : 'core_text';
  }
}

interface Seg {
  kind: 'call' | 'gap';
  s: number; // ms relative to session start
  e: number;
  entry?: CallEntry;
  // Gap only: tool_calls of the call that CLOSES this gap — i.e. the number of
  // tool_result blocks at the tail of the next request, which is how many tools
  // the client ran during this gap. 0 for a text-turn gap (waiting on the user)
  // or the terminal gap (session ends, no closing call). Drives the (n+1)-block
  // even-split estimate — see the gap render + Tooltip.
  toolN?: number;
}

interface Lane {
  agentId: string; // '' for the main session
  t0: number; // strip start (ms rel.)
  t1: number; // strip end (ms rel.)
  segs: Seg[];
}

// Wall-clock decomposition over the main lane (which covers all of [0, span]).
// Fields are milliseconds and sum to span. api and sub are measured; tool and
// idle are ESTIMATES from the even (n+1)-block split of each gap.
interface Roll {
  api: number;
  tool: number;
  idle: number;
  sub: number;
}

interface TipState {
  seg: Seg;
  main: boolean;
  rect: DOMRect;
}

const clock = (ms: number, base: number) => new Date(base + ms).toLocaleTimeString([], { hour12: false });

// Partition [T0,T1] into abutting call/gap segments from a lane's calls. Each gap
// is emitted immediately before the call that closes it, and carries that call's
// tool_calls as toolN so the gap can be decomposed into tool vs idle time.
function segsFor(rows: { s: number; e: number; entry: CallEntry }[], T0: number, T1: number): Seg[] {
  const sorted = [...rows].sort((a, b) => a.s - b.s);
  const segs: Seg[] = [];
  let cur = T0;
  for (const r of sorted) {
    if (r.s > cur) segs.push({ kind: 'gap', s: cur, e: r.s, toolN: Math.max(0, r.entry.tool_calls) });
    const s = Math.max(r.s, cur);
    segs.push({ kind: 'call', s, e: Math.max(r.e, s), entry: r.entry });
    cur = Math.max(cur, r.e);
  }
  if (cur < T1) segs.push({ kind: 'gap', s: cur, e: T1, toolN: 0 }); // terminal gap → idle (session ends)
  return segs;
}

export function SessionTimeline({ entries }: { entries: CallEntry[] }) {
  const [tip, setTip] = useState<TipState | null>(null);

  const model = useMemo(() => {
    const timed = entries
      .map((e) => {
        const s = new Date(e.ts).getTime();
        const end = e.ts_end ? new Date(e.ts_end).getTime() : s;
        return { entry: e, start: s, end: Math.max(end, s) };
      })
      .filter((r) => Number.isFinite(r.start))
      .sort((a, b) => a.start - b.start);
    if (timed.length === 0) return null;

    const base = timed[0].start;
    const span = Math.max(...timed.map((r) => r.end)) - base || 1;

    // Group into lanes by agent_id; main ('') first, then sub-agents by first call.
    const byAgent = new Map<string, { s: number; e: number; entry: CallEntry }[]>();
    for (const r of timed) {
      const rel = { s: r.start - base, e: r.end - base, entry: r.entry };
      const key = r.entry.agent_id || '';
      (byAgent.get(key) ?? byAgent.set(key, []).get(key)!).push(rel);
    }
    const agentIds = [...byAgent.keys()].sort((a, b) => {
      if (a === '') return -1;
      if (b === '') return 1;
      return Math.min(...byAgent.get(a)!.map((r) => r.s)) - Math.min(...byAgent.get(b)!.map((r) => r.s));
    });

    // Sub-agent active intervals, to label the main lane's waiting gaps.
    const subIntervals = agentIds
      .filter((k) => k !== '')
      .flatMap((k) => byAgent.get(k)!.map((r) => [r.s, r.e] as const));

    const lanes: Lane[] = agentIds.map((agentId) => {
      const rows = byAgent.get(agentId)!;
      const t0 = agentId === '' ? 0 : Math.min(...rows.map((r) => r.s));
      const t1 = agentId === '' ? span : Math.max(...rows.map((r) => r.e));
      return { agentId, t0, t1, segs: segsFor(rows, t0, t1) };
    });

    // Time decomposition over the main lane. A gap overlapping a sub-agent's active
    // window is "waiting on sub-agent" (that time is the sub-agent running, already
    // decomposed in its own lane) and is NOT split into tool/idle here.
    const overlapsSub = (s: number, e: number) => subIntervals.some(([a, b]) => s < b && e > a);
    const roll: Roll = { api: 0, tool: 0, idle: 0, sub: 0 };
    const mainLane = lanes.find((l) => l.agentId === '');
    if (mainLane) {
      for (const seg of mainLane.segs) {
        const d = seg.e - seg.s;
        if (seg.kind === 'call') {
          roll.api += d;
        } else if (overlapsSub(seg.s, seg.e)) {
          roll.sub += d;
        } else {
          const n = seg.toolN ?? 0;
          roll.tool += (n / (n + 1)) * d;
          roll.idle += (1 / (n + 1)) * d;
        }
      }
    }

    const subCount = agentIds.filter((k) => k !== '').length;
    return { base, span, lanes, subIntervals, subCount, roll };
  }, [entries]);

  if (!model) return null;
  const { base, span, lanes, subIntervals, roll } = model;

  // Ruler ticks — pick a step that yields ~5–8 marks.
  const stepMs = niceStep(span);
  const ticks: number[] = [];
  for (let t = 0; t <= span; t += stepMs) ticks.push(t);

  const gapIsWaiting = (seg: Seg) => subIntervals.some(([s, e]) => seg.s < e && seg.e > s);

  return (
    <>
      <div className={styles.scroll}>
        <div className={styles.grid}>
          <div className={styles.ruler}>
            {ticks.map((t) => (
              <div key={t} className={styles.tick} style={{ left: `${(t / span) * 100}%` }}>
                <span>{Math.round(t / 60000)}m</span>
              </div>
            ))}
          </div>

          <div className={styles.lanes}>
            {lanes.map((lane) => {
              const stripDur = Math.max(lane.t1 - lane.t0, 1);
              const main = lane.agentId === '';
              return (
                <div key={lane.agentId || 'main'} className={styles.lane}>
                  <div
                    className={styles.strip}
                    style={{ left: `${(lane.t0 / span) * 100}%`, width: `${((lane.t1 - lane.t0) / span) * 100}%` }}
                  >
                    {lane.segs.map((seg, i) => {
                      const width = `${((seg.e - seg.s) / stripDur) * 100}%`;
                      if (seg.kind === 'gap') {
                        const enter = (ev: React.MouseEvent<HTMLElement>) =>
                          setTip({ seg, main, rect: ev.currentTarget.getBoundingClientRect() });
                        const leave = () => setTip(null);
                        const n = seg.toolN ?? 0;
                        const waiting = main && gapIsWaiting(seg);
                        // Split into n tool slices + 1 idle slice (even 1/(n+1) each),
                        // unless it's a sub-agent wait or a pure-idle gap (n === 0),
                        // which render as a single neutral segment.
                        if (waiting || n === 0) {
                          return (
                            <div
                              key={i}
                              className={`${styles.seg} ${styles.gap}`}
                              style={{ width }}
                              onMouseEnter={enter}
                              onMouseLeave={leave}
                            />
                          );
                        }
                        return (
                          <div
                            key={i}
                            className={styles.gapSplit}
                            style={{ width }}
                            onMouseEnter={enter}
                            onMouseLeave={leave}
                          >
                            {Array.from({ length: n }, (_, k) => (
                              <span key={k} className={styles.toolEst} />
                            ))}
                            <span className={styles.idleEst} />
                          </div>
                        );
                      }
                      const cat = CATS[categorize(seg.entry!)];
                      return (
                        <div
                          key={i}
                          tabIndex={0}
                          className={`${styles.seg} ${styles.call} ${cat.hatch ? styles.error : ''}`}
                          style={{ width, '--bc': cat.color } as CSSProperties}
                          onMouseEnter={(ev) => setTip({ seg, main, rect: ev.currentTarget.getBoundingClientRect() })}
                          onMouseLeave={() => setTip(null)}
                          onFocus={(ev) => setTip({ seg, main, rect: ev.currentTarget.getBoundingClientRect() })}
                          onBlur={() => setTip(null)}
                        />
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </div>

      <TimeRollup roll={roll} span={span} />

      {tip ? <Tooltip tip={tip} base={base} gapIsWaiting={gapIsWaiting} /> : null}
    </>
  );
}

const ROLL_PARTS: { key: keyof Roll; label: string; color: string; est?: boolean }[] = [
  { key: 'api', label: 'In-flight (API)', color: 'var(--chart-2)' },
  { key: 'tool', label: 'Tool runs (est)', color: 'var(--chart-1)', est: true },
  { key: 'idle', label: 'Idle (est)', color: 'var(--border)', est: true },
  { key: 'sub', label: 'Sub-agent', color: 'var(--chart-3)' },
];

// Where the wall-clock went: measured in-flight (API) + sub-agent time, plus the
// even-split estimate of tool vs idle across the gaps. Hatched segments/dots mark
// the estimated (modeled, not measured) portions.
function TimeRollup({ roll, span }: { roll: Roll; span: number }) {
  const total = span || 1;
  const parts = ROLL_PARTS.filter((p) => roll[p.key] > 0);
  if (parts.length === 0) return null;
  return (
    <div className={styles.rollup}>
      <div className={styles.rollBar}>
        {parts.map((p) => (
          <span
            key={p.key}
            className={`${styles.rollSeg} ${p.est ? styles.rollEst : ''}`}
            style={{ width: `${(roll[p.key] / total) * 100}%`, '--bc': p.color } as CSSProperties}
            title={`${p.label} · ${duration(roll[p.key] / 1000)}`}
          />
        ))}
      </div>
      <div className={styles.rollLegend}>
        {parts.map((p) => (
          <span key={p.key} className={styles.rollItem}>
            <span
              className={`${styles.rollDot} ${p.est ? styles.rollEst : ''}`}
              style={{ '--bc': p.color } as CSSProperties}
            />
            {p.label}
            <b>{percent(roll[p.key] / total)}</b>
            <span className={styles.rollDur}>{duration(roll[p.key] / 1000)}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

function Tooltip({ tip, base, gapIsWaiting }: { tip: TipState; base: number; gapIsWaiting: (s: Seg) => boolean }) {
  const { seg, rect } = tip;
  const left = Math.max(8, Math.min(rect.left + rect.width / 2, window.innerWidth - 8));
  const top = rect.top - 8;
  const style: CSSProperties = { left, top, transform: 'translate(-50%, -100%)' };

  if (seg.kind === 'call' && seg.entry) {
    const e = seg.entry;
    const cat = CATS[categorize(e)];
    const status = e.status === 200 ? '200 OK' : e.status <= 0 ? `${e.status} · aborted` : e.status;
    const out = e.output_tokens || 0;
    const cacheRead = e.cache_read_input_tokens || 0;
    return (
      <div className={styles.tip} style={style} role="tooltip">
        <div className={styles.tipHead}>
          <span className={styles.sw} style={{ '--bc': cat.color } as CSSProperties} />
          {cat.label}
        </div>
        <dl className={styles.tipRows}>
          <dt>Time</dt>
          <dd>{clock(seg.s, base)}</dd>
          <dt>Duration</dt>
          <dd>{duration((seg.e - seg.s) / 1000)}</dd>
          <dt>Output</dt>
          <dd>{int(out)} tok</dd>
          {cacheRead ? (
            <>
              <dt>Cache read</dt>
              <dd>{int(cacheRead)} tok</dd>
            </>
          ) : null}
          <dt>Tool calls</dt>
          <dd>{e.tool_calls}</dd>
          <dt>Status</dt>
          <dd>{status}</dd>
        </dl>
      </div>
    );
  }

  const waiting = tip.main && gapIsWaiting(seg);
  const n = seg.toolN ?? 0;
  const durSec = (seg.e - seg.s) / 1000;
  const head = waiting ? 'Waiting on sub-agent' : n > 0 ? 'Client processing' : 'Idle';
  return (
    <div className={styles.tip} style={style} role="tooltip">
      <div className={styles.tipHead}>
        <span className={styles.sw} style={{ '--bc': 'var(--border)' } as CSSProperties} />
        {head}
      </div>
      <dl className={styles.tipRows}>
        <dt>From</dt>
        <dd>{clock(seg.s, base)}</dd>
        <dt>Duration</dt>
        <dd>{duration(durSec)}</dd>
        {!waiting && n > 0 ? (
          <>
            <dt>Tool est · {n}</dt>
            <dd>{duration((n / (n + 1)) * durSec)}</dd>
            <dt>Idle est</dt>
            <dd>{duration((1 / (n + 1)) * durSec)}</dd>
          </>
        ) : null}
      </dl>
      <div className={styles.tipNote}>
        {waiting
          ? 'Main loop blocked while the sub-agent runs.'
          : n > 0
            ? `Even split: ${n} tool run${n > 1 ? 's' : ''} + idle, 1/${n + 1} each (v1 estimate).`
            : 'No request in flight — waiting on the user.'}
      </div>
    </div>
  );
}

// niceStep returns a round tick interval (ms) giving roughly 6–9 marks over the span.
function niceStep(spanMs: number): number {
  const target = spanMs / 8;
  const steps = [
    15_000, 30_000, 60_000, 2 * 60_000, 5 * 60_000, 10 * 60_000, 15 * 60_000, 30 * 60_000, 60 * 60_000,
  ];
  for (const s of steps) if (s >= target) return s;
  return steps[steps.length - 1];
}
