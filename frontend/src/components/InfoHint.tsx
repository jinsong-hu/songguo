import type { ReactNode } from 'react';
import styles from './InfoHint.module.css';

/**
 * Shared caveat for every view built from LOCAL token counts (compose): the
 * proportions are stable and exact, but the absolute numbers are our own
 * tokenizer's, not the vendor's. Kept in one place so the wording never drifts.
 */
export const ESTIMATE_HINT =
  'Counted locally with the o200k_base tokenizer (tiktoken), not the vendor’s own ' +
  'tokenizer. Proportions and growth are stable and exact — an unchanged prompt always ' +
  'counts the same — but absolute token counts won’t match the official usage numbers ' +
  '(those stay authoritative for billing).';

/** A small `?` badge that reveals an explanatory tooltip on hover/focus. */
export function InfoHint({
  text = ESTIMATE_HINT,
  content,
  label = 'About these numbers',
}: {
  text?: string;
  content?: ReactNode;
  label?: string;
}) {
  return (
    <span className={styles.hint} tabIndex={0} role="note" aria-label={`${label}: ${text}`}>
      <span aria-hidden="true" className={styles.badge}>?</span>
      <span className={styles.tip} role="tooltip">{content ?? text}</span>
    </span>
  );
}
