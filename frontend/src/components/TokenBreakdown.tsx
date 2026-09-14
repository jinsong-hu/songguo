import { useState } from 'react';
import type { CallEntry } from '../api/types';
import { InfoHint } from './InfoHint';
import { int, money } from '../lib/format';
import styles from './TokenBreakdown.module.css';

type Usage = Pick<
  CallEntry,
  'input_tokens' | 'cache_read_input_tokens' | 'cache_creation_input_tokens' | 'output_tokens' | 'thinking_tokens' | 'cost'
>;

interface Segment {
  key: string;
  label: string;
  tokens: number;
  color: string;
}

const USAGE_HINT =
  'Provider-reported usage, as billed. Input is split by cache status: fresh, cache read and ' +
  'cache write never overlap and add up to the whole prompt. Thinking is part of output, ' +
  'never counted on top of it.';

/**
 * TokenBreakdown draws a call's billed usage as two part-to-whole bars, one per
 * direction, so the accounting reads at a glance: input = fresh + cache read +
 * cache write, output = thinking + reply. Each bar is scaled to its own total —
 * the totals beside them carry the magnitude.
 */
export function TokenBreakdown({ usage }: { usage: Usage }) {
  const inputTotal = usage.input_tokens + usage.cache_read_input_tokens + usage.cache_creation_input_tokens;
  const thinking = Math.min(usage.thinking_tokens, usage.output_tokens);

  // Bar order keeps the two cached slices apart from each other's neighbour
  // hues; see the palette note in TokenBreakdown.module.css.
  const input: Segment[] = [
    { key: 'cache_read', label: 'Cache read', tokens: usage.cache_read_input_tokens, color: 'var(--chart-1)' },
    { key: 'cache_write', label: 'Cache write', tokens: usage.cache_creation_input_tokens, color: 'var(--chart-4)' },
    { key: 'fresh', label: 'Fresh', tokens: usage.input_tokens, color: 'var(--chart-3)' },
  ];
  const output: Segment[] = [
    { key: 'thinking', label: 'Thinking', tokens: thinking, color: 'var(--chart-5)' },
    { key: 'reply', label: 'Reply', tokens: usage.output_tokens - thinking, color: 'var(--producer-1)' },
  ];

  return (
    <div className={`card ${styles.card}`}>
      <div className={styles.head}>
        <span className={styles.title}>
          Tokens
          <InfoHint text={USAGE_HINT} label="How tokens are counted" />
        </span>
        <span className={styles.total}>
          {int(inputTotal + usage.output_tokens)} total · {money(usage.cost)}
        </span>
      </div>
      <TokenRow name="Input" total={inputTotal} segments={input} />
      <TokenRow name="Output" total={usage.output_tokens} segments={output} />
    </div>
  );
}

function TokenRow({ name, total, segments }: { name: string; total: number; segments: Segment[] }) {
  const [hover, setHover] = useState<string | null>(null);
  const present = segments.filter((s) => s.tokens > 0);
  const pct = (tokens: number) => (total > 0 ? (tokens / total) * 100 : 0);

  return (
    <div className={styles.row}>
      <div className={styles.rowHead}>
        <div className={styles.rowName}>{name}</div>
        <div className={styles.rowTotal}>{int(total)}</div>
        {present.length > 1 ? (
          <div className={styles.equation}>= {present.map((s) => s.label.toLowerCase()).join(' + ')}</div>
        ) : null}
      </div>
      <div className={styles.rowBody}>
        <div className={styles.bar} role="img" aria-label={`${name} ${int(total)} tokens: ${present.map((s) => `${s.label} ${int(s.tokens)}`).join(', ') || 'none'}`}>
          {present.length === 0 ? <div className={styles.barEmpty} /> : null}
          {present.map((s) => (
            <div
              key={s.key}
              className={`${styles.segment} ${hover && hover !== s.key ? styles.dim : ''}`}
              style={{ flexGrow: s.tokens, background: s.color }}
              title={`${s.label}: ${int(s.tokens)} tokens (${formatPct(pct(s.tokens))})`}
              onMouseEnter={() => setHover(s.key)}
              onMouseLeave={() => setHover(null)}
            />
          ))}
        </div>
        <ul className={styles.legend}>
          {segments.map((s) => (
            <li
              key={s.key}
              className={`${styles.legendItem} ${s.tokens === 0 ? styles.legendZero : ''} ${hover && hover !== s.key ? styles.dim : ''}`}
              onMouseEnter={() => s.tokens > 0 && setHover(s.key)}
              onMouseLeave={() => setHover(null)}
            >
              <span className={styles.swatch} style={{ background: s.color }} />
              <span className={styles.legendLabel}>{s.label}</span>
              <span className={styles.legendValue}>{s.tokens > 0 ? int(s.tokens) : '—'}</span>
              {s.tokens > 0 ? <span className={styles.legendPct}>{formatPct(pct(s.tokens))}</span> : null}
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

function formatPct(share: number): string {
  if (share > 0 && share < 1) return '<1%';
  if (share < 100 && share > 99) return '>99%';
  return `${Math.round(share)}%`;
}
