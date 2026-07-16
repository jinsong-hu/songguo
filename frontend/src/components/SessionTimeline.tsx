import { useMemo, useState, type CSSProperties } from 'react';
import type { CallEntry } from '../api/types';
import { duration, int } from '../lib/format';
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
}

interface Lane {
  agentId: string; // '' for the main session
  t0: number; // strip start (ms rel.)
  t1: number; // strip end (ms rel.)
  segs: Seg[];
}

interface TipState {
  seg: Seg;
  main: boolean;
  rect: DOMRect;
}

const clock = (ms: number, base: number) => new Date(base + ms).toLocaleTimeString([], { hour12: false });

// Partition [T0,T1] into abutting call/gap segments from a lane's calls.
function segsFor(rows: { s: number; e: number; entry: CallEntry }[], T0: number, T1: number): Seg[] {
  const sorted = [...rows].sort((a, b) => a.s - b.s);
  const segs: Seg[] = [];
  let cur = T0;
  for (const r of sorted) {
    if (r.s > cur) segs.push({ kind: 'gap', s: cur, e: r.s });
    const s = Math.max(r.s, cur);
    segs.push({ kind: 'call', s, e: Math.max(r.e, s), entry: r.entry });
    cur = Math.max(cur, r.e);
  }
  if (cur < T1) segs.push({ kind: 'gap', s: cur, e: T1 });
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

    const subCount = agentIds.filter((k) => k !== '').length;
    return { base, span, lanes, subIntervals, subCount };
  }, [entries]);

  if (!model) return null;
  const { base, span, lanes, subIntervals } = model;

  // Ruler ticks — pick a step that yields ~5–8 marks.
  const stepMs = niceStep(span);
  const ticks: number[] = [];
  for (let t = 0; t <= span; t += stepMs) ticks.push(t);

  const gapIsWaiting = (seg: Seg) => subIntervals.some(([s, e]) => seg.s < e && seg.e > s);

  return (
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
                      return (
                        <div
                          key={i}
                          className={`${styles.seg} ${styles.gap}`}
                          style={{ width }}
                          onMouseEnter={(ev) => setTip({ seg, main, rect: ev.currentTarget.getBoundingClientRect() })}
                          onMouseLeave={() => setTip(null)}
                        />
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

      {tip ? <Tooltip tip={tip} base={base} gapIsWaiting={gapIsWaiting} /> : null}
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
  return (
    <div className={styles.tip} style={style} role="tooltip">
      <div className={styles.tipHead}>
        <span className={styles.sw} style={{ '--bc': 'var(--border)' } as CSSProperties} />
        {waiting ? 'Waiting on sub-agent' : 'Client processing'}
      </div>
      <dl className={styles.tipRows}>
        <dt>From</dt>
        <dd>{clock(seg.s, base)}</dd>
        <dt>Duration</dt>
        <dd>{duration((seg.e - seg.s) / 1000)}</dd>
      </dl>
      <div className={styles.tipNote}>
        {waiting ? 'Main loop blocked while the sub-agent runs.' : 'Local work — tool run or user, no request in flight.'}
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
