import { Link, useParams } from 'react-router-dom';
import { ArrowLeft, FileText, GitBranch } from 'lucide-react';
import { api } from '../api/client';
import { CopyButton } from '../components/CopyButton';
import { ConfidenceDot } from '../components/ConfidenceDot';
import { ContextSunburst } from '../components/ContextSunburst';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { ESTIMATE_HINT, InfoHint } from '../components/InfoHint';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { StatusPill } from '../components/StatusPill';
import { TracePanel } from '../components/TracePanel';
import { useFetch } from '../lib/useFetch';
import { dateTime, int, money, ms } from '../lib/format';
import styles from './Detail.module.css';

// shortId renders a UUID as a compact prefix for titles; the full id is still
// copyable from the trace card.
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

export function RequestDetailPage() {
  const { id = '' } = useParams();
  // The call id is a UUID string, used as-is (no numeric coercion).
  const callId = id;
  const valid = id !== '';

  const { data, error, initialLoading, refetch } = useFetch(
    () => api.call(callId),
    [callId],
    { enabled: valid },
  );

  return (
    <Page
      title={`Request ${shortId(id)}`}
      actions={
        <Link to="/" className="btn">
          <ArrowLeft size={15} /> Back to overview
        </Link>
      }
    >
      {!valid || error ? (
        error ? (
          <ErrorBanner message={error} onRetry={refetch} />
        ) : (
          <EmptyState icon={FileText} title="Request not found" />
        )
      ) : initialLoading || !data ? (
        <div className={styles.stack}>
          <Skeleton height={120} />
          <Skeleton height={220} />
        </div>
      ) : (
        <div className={styles.stack}>
          {data.session_id && (
            <div className={`card ${styles.banner}`}>
              <GitBranch size={15} />
              <span>Part of an agent session.</span>
              <Link to={`/sessions/${encodeURIComponent(data.session_id)}`} className="btn btn-sm">
                View session
              </Link>
            </div>
          )}

          <div className={styles.grid}>
            <Field label="Time">{dateTime(data.ts)}</Field>
            <Field label="Model" mono>{data.model || '—'}</Field>
            <Field label="Modality">
              <span className="chip" style={{ textTransform: 'capitalize' }}>
                {data.modality || '—'}
              </span>
            </Field>
            <Field label="Vendor">{data.vendor || '—'}</Field>
            <Field label="Wire" mono>{data.wire || '—'}</Field>
            <Field label="Status">
              <StatusPill status={data.status} />
            </Field>
            <Field label="Cost">{money(data.cost)}</Field>
            <Field label="Latency">{ms(data.latency_ms)}</Field>
            <Field label="TTFT">{data.ttft_ms > 0 ? ms(data.ttft_ms) : '—'}</Field>
            <Field label="Output speed">
              {data.output_tokens_per_second > 0 ? `${data.output_tokens_per_second.toFixed(1)} tok/s` : '—'}
            </Field>
            <Field label="Confidence">
              <ConfidenceDot confidence={data.confidence} />
            </Field>
            <Field label="Stream">{data.stream ? 'yes' : 'no'}</Field>
            <Field label="Input tokens">{int(data.input_tokens)}</Field>
            {data.cache_read_input_tokens > 0 && (
              <Field label="Cache read">{int(data.cache_read_input_tokens)}</Field>
            )}
            {data.cache_creation_input_tokens > 0 && (
              <Field label="Cache creation">{int(data.cache_creation_input_tokens)}</Field>
            )}
            <Field label="Output tokens">{int(data.output_tokens)}</Field>
            {data.thinking_tokens > 0 && (
              <Field label="Thinking tokens">{int(data.thinking_tokens)}</Field>
            )}
            <Field label="User">{data.user_id || '—'}</Field>
            {data.agent_id && <Field label="Agent" mono>{data.agent_id}</Field>}
            {data.parent_agent_id && <Field label="Parent agent" mono>{data.parent_agent_id}</Field>}
          </div>

          {data.err && (
            <div className={`card ${styles.errCard}`}>
              <div className={styles.fieldLabel}>Error</div>
              <pre className={styles.errText}>{data.err}</pre>
            </div>
          )}

          {Object.keys(data.tags).length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel}>Tags</div>
              <div className={styles.tags}>
                {Object.entries(data.tags).map(([k, v]) => (
                  <span key={k} className="chip chip-mono">
                    {k}={v}
                  </span>
                ))}
              </div>
            </div>
          )}

          {data.composition && data.composition.sources.length > 0 && (
            <div className="card" style={{ padding: 16 }}>
              <div className={styles.fieldLabel} style={{ marginBottom: 12, display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                Context distribution
                <InfoHint
                  text={`${ESTIMATE_HINT} This request chart shows the single input context window for this request.`}
                  content={
                    <>
                      <span>This request chart shows the single input context window for this request.</span>
                      <span style={{ display: 'block', marginTop: 8 }}>{ESTIMATE_HINT}</span>
                    </>
                  }
                />
              </div>
              <ContextSunburst data={{ sources: data.composition.sources }} centerLabel="window" />
            </div>
          )}

          <div className="card" style={{ padding: 16 }}>
            <div className={styles.traceHead}>
              <span className={styles.fieldLabel}>Captured trace</span>
              <CopyButton value={String(data.id)} label="Copy call id" />
            </div>
            <TracePanel entry={data} />
          </div>
        </div>
      )}
    </Page>
  );
}

function Field({
  label,
  children,
  mono,
}: {
  label: string;
  children: React.ReactNode;
  mono?: boolean;
}) {
  return (
    <div className={styles.field}>
      <div className={styles.fieldLabel}>{label}</div>
      <div className={mono ? `${styles.fieldValue} mono` : styles.fieldValue}>{children}</div>
    </div>
  );
}
