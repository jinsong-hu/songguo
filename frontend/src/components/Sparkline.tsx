import { useMemo, useRef, useState, type PointerEvent } from 'react';
import styles from './Sparkline.module.css';

const H = 36;

const timeFmt = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

/**
 * A single-series trend for a stat tile: the recent history in a recessive
 * line, the current value as an accent dot, and a crosshair tooltip on hover.
 *
 * Null values are gaps, not zeros — a reading that was not taken must not draw a
 * dip to the floor. `max` pins the top of the scale (100 for a percentage) so a
 * quiet line stays quiet instead of being stretched to fill the box; `line`
 * draws a dashed threshold.
 */
export function Sparkline({
  t,
  values,
  format,
  label,
  max,
  line,
}: {
  t: number[];
  values: (number | null)[];
  format: (v: number) => string;
  label: string;
  max?: number;
  line?: number;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<number | null>(null);

  const geom = useMemo(() => {
    const n = values.length;
    const present = values.filter((v): v is number => v !== null && Number.isFinite(v));
    const top = Math.max(max ?? 0, line ? line * 1.15 : 0, ...present, 1e-9);
    const x = (i: number) => (n <= 1 ? 100 : (i / (n - 1)) * 100);
    const y = (v: number) => H - 2 - (Math.min(v, top) / top) * (H - 4);
    let d = '';
    let pen = false;
    values.forEach((v, i) => {
      if (v === null || !Number.isFinite(v)) {
        pen = false;
        return;
      }
      d += `${pen ? 'L' : 'M'}${x(i).toFixed(2)},${y(v).toFixed(2)}`;
      pen = true;
    });
    let last = -1;
    for (let i = n - 1; i >= 0; i -= 1) {
      if (values[i] !== null && Number.isFinite(values[i])) {
        last = i;
        break;
      }
    }
    return { d, x, y, last, lineY: line ? y(line) : null };
  }, [values, max, line]);

  if (values.length < 2 || geom.d === '') {
    return <div className={styles.empty}>Not enough samples yet</div>;
  }

  const onMove = (e: PointerEvent<HTMLDivElement>) => {
    const box = ref.current?.getBoundingClientRect();
    if (!box || box.width === 0) return;
    const f = Math.min(1, Math.max(0, (e.clientX - box.left) / box.width));
    setHover(Math.round(f * (values.length - 1)));
  };

  const focus = hover ?? geom.last;
  const focusValue = focus >= 0 ? values[focus] : null;

  return (
    <div
      ref={ref}
      className={styles.wrap}
      onPointerMove={onMove}
      onPointerLeave={() => setHover(null)}
      role="img"
      aria-label={`${label}, last ${Math.round((t[t.length - 1] - t[0]) / 60000)} minutes`}
    >
      <svg className={styles.svg} viewBox={`0 0 100 ${H}`} preserveAspectRatio="none" aria-hidden="true">
        {geom.lineY !== null && (
          <line x1="0" x2="100" y1={geom.lineY} y2={geom.lineY} className={styles.threshold} />
        )}
        <path d={geom.d} className={styles.path} />
        {hover !== null && (
          <line x1={geom.x(hover)} x2={geom.x(hover)} y1="0" y2={H} className={styles.crosshair} />
        )}
      </svg>
      {focusValue !== null && focus >= 0 && (
        <span
          className={hover === null ? styles.dot : `${styles.dot} ${styles.dotHover}`}
          style={{ left: `${geom.x(focus)}%`, top: `${(geom.y(focusValue) / H) * 100}%` }}
        />
      )}
      {hover !== null && (
        <div
          className={styles.tip}
          style={{ left: `${Math.min(80, Math.max(20, geom.x(hover)))}%` }}
          role="tooltip"
        >
          <span className={styles.tipTime}>{timeFmt.format(new Date(t[hover]))}</span>
          <span className={styles.tipValue}>{focusValue === null ? '—' : format(focusValue)}</span>
        </div>
      )}
    </div>
  );
}
