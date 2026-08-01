import { outcomeOf, outcomeLabel, outcomeStyle, outcomeTone, type OutcomeInput } from '../lib/outcome';

/**
 * A colored pill naming what happened to a call.
 *
 * It takes the whole outcome, not a bare status int, because the status alone is
 * ambiguous: -1 is an internal in-flight sentinel that must never be shown, and
 * a 402/404/502 minted by songguo means something different from the same code
 * returned by a provider. Where the provider's code IS the answer (a 2xx, or the
 * provider's own error) the pill shows that code verbatim.
 */
export function StatusPill({ call }: { call: OutcomeInput }) {
  const outcome = outcomeOf(call);
  const tone = outcomeTone(outcome, call.status);
  return (
    <span className={`pill pill-${tone}`} title={outcomeStyle(outcome).hint}>
      <span className="dot" />
      {outcomeLabel(outcome, call.status)}
    </span>
  );
}
