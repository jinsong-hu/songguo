import { api } from '../api/client';
import type { CallEntry, CallTrace, TraceSide } from '../api/types';
import { CopyButton } from './CopyButton';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import { useFetch } from '../lib/useFetch';
import styles from '../pages/Overview.module.css';

/**
 * TracePanel renders the captured request/response payload for a call, lazily
 * fetched. Shared by the request-detail page (and previously the inline calls
 * table). Shows a note when no payload was captured.
 */
export function TracePanel({ entry }: { entry: CallEntry }) {
  const trace = useFetch<CallTrace>(() => api.trace(entry.id), [entry.id], {
    enabled: entry.has_trace,
  });

  if (!entry.has_trace) {
    return (
      <div className={styles.traceNote}>
        No captured payload — capture is off, or this call predates it.
      </div>
    );
  }

  if (trace.error) {
    return (
      <div className={styles.tracePanel}>
        <ErrorBanner message={trace.error} onRetry={trace.refetch} />
      </div>
    );
  }

  if (trace.initialLoading || !trace.data) {
    return (
      <div className={styles.tracePanel}>
        <div className={styles.traceGrid}>
          {['Request', 'Response'].map((side) => (
            <div key={side} className={styles.traceSide}>
              <div className={styles.traceSideHead}>{side}</div>
              <Skeleton height={14} style={{ marginBottom: 8 }} />
              <Skeleton height={80} />
            </div>
          ))}
        </div>
      </div>
    );
  }

  return (
    <div className={styles.tracePanel}>
      <div className={styles.traceGrid}>
        <TraceSidePane title="Request" side={trace.data.request} />
        <TraceSidePane title="Response" side={trace.data.response} />
      </div>
    </div>
  );
}

/** Pretty-print JSON bodies with 2-space indent; fall back to raw text. */
function prettyBody(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2);
  } catch {
    return body;
  }
}

function TraceSidePane({ title, side }: { title: string; side: TraceSide }) {
  const headerEntries = Object.entries(side.headers);
  const display = side.body_base64 ? side.body : prettyBody(side.body);
  return (
    <div className={styles.traceSide}>
      <div className={styles.traceSideHead}>
        <span>{title}</span>
        {side.content_type && (
          <span className="chip chip-mono">{side.content_type}</span>
        )}
        {side.body_base64 && (
          <span className={`chip ${styles.binaryChip}`}>binary (base64)</span>
        )}
      </div>

      {headerEntries.length > 0 && (
        <dl className={styles.headerList}>
          {headerEntries.map(([k, v]) => (
            <div key={k} className={styles.headerItem}>
              <dt className={styles.headerKey}>{k}</dt>
              <dd className={styles.headerVal}>{v}</dd>
            </div>
          ))}
        </dl>
      )}

      <div className={styles.bodyWrap}>
        <div className={styles.bodyActions}>
          <CopyButton value={side.body} className={styles.copyBody} />
        </div>
        <pre className={styles.bodyCode}>
          {display || <span className="muted">(empty)</span>}
        </pre>
      </div>
    </div>
  );
}
