import { AlertTriangle, Clock, Gauge, Link2 } from 'lucide-react';
import type { Provider } from '../api/types';
import styles from './RoutingPanel.module.css';

/**
 * Live routing state for one provider: what the gateway will do with the NEXT
 * request, as opposed to the historical stats derived from the call ledger.
 *
 * This is where an operator lands after clicking a degraded provider card, so
 * unlike the card — which has room for one badge — it answers the follow-up
 * questions: which of this provider's vendors is degraded, how long the demotion
 * lasts, and whether the concurrency limit is actually being hit.
 */
export function RoutingPanel({ provider }: { provider: Provider }) {
  const { routing, capacity } = provider;

  // A provider with no router opinion and no configured limit has nothing to
  // report; showing an empty panel would just be noise.
  const hasCapacity = capacity.limit > 0 || capacity.in_flight > 0 || capacity.waiting > 0;
  if (!routing && !hasCapacity) return null;

  const degraded = routing?.degraded ?? [];

  return (
    <section className={`card ${styles.panel}`}>
      <header className={styles.head}>
        <h2 className={styles.title}>Live routing</h2>
        <span className={styles.subtitle}>What the next request will do — not history.</span>
      </header>

      {routing?.dead && (
        <div className={`${styles.callout} ${styles.calloutDead}`}>
          <AlertTriangle size={16} className={styles.calloutIcon} />
          <div>
            <strong>Presumed dead.</strong> This provider failed repeatedly with no success
            in between, so it is ranked below every working provider.{' '}
            <em>This does not clear on a timer.</em> While any healthy provider serves this
            model, requests will not be sent here — so it stays down until a request does
            succeed, or until you disable it and fix the cause.
          </div>
        </div>
      )}

      {!routing?.dead && routing?.cooling && (
        <div className={`${styles.callout} ${styles.calloutCooling}`}>
          <Clock size={16} className={styles.calloutIcon} />
          <div>
            <strong>Cooling down.</strong> Recent failures demoted this provider. It is
            ranked last for now and recovers on its own when the cooldown lapses — no
            action needed. Repeated demotions back off further each time.
          </div>
        </div>
      )}

      <dl className={styles.stats}>
        {hasCapacity && (
          <div className={styles.stat}>
            <dt className={styles.statLabel}>
              <Gauge size={13} /> Concurrency
            </dt>
            <dd className={styles.statValue}>
              {capacity.limit > 0
                ? `${capacity.in_flight} / ${capacity.limit}`
                : `${capacity.in_flight}`}
              {capacity.limit === 0 && <span className={styles.statNote}>unlimited</span>}
            </dd>
          </div>
        )}

        {hasCapacity && (
          <div className={styles.stat}>
            <dt className={styles.statLabel}>Queued</dt>
            <dd className={capacity.waiting > 0 ? styles.statValueWarn : styles.statValue}>
              {capacity.waiting}
              {capacity.waiting > 0 && (
                <span className={styles.statNote}>waiting for a slot</span>
              )}
            </dd>
          </div>
        )}

        <div className={styles.stat}>
          <dt className={styles.statLabel}>
            <Link2 size={13} /> Pinned sessions
          </dt>
          <dd className={styles.statValue}>{routing?.sessions ?? 0}</dd>
        </div>
      </dl>

      {degraded.length > 0 && (
        <p className={styles.note}>
          Affected {degraded.length === 1 ? 'endpoint' : 'endpoints'}:{' '}
          <code className={styles.code}>{degraded.join(', ')}</code>. A provider serving
          several protocols is routed as one entry per host, and those hosts fail
          independently — the rest of this provider may still be serving normally.
        </p>
      )}

      {capacity.waiting > 0 && (
        <p className={styles.note}>
          Requests are queueing. They <strong>wait</strong> rather than moving to another
          provider, which would discard the session&rsquo;s prompt cache — but a queue
          that is never empty means this limit is lower than what the provider actually
          allows.
        </p>
      )}
    </section>
  );
}
