import { api } from '../api/client';
import type { CallEntry, CallTrace, TraceSide } from '../api/types';
import { CopyButton } from './CopyButton';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import { displayBody } from '../lib/traceBody';
import { useFetch } from '../lib/useFetch';
import styles from '../pages/ActivityFeed.module.css';

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
    // An in-flight call cannot predate capture — with capture on its request
    // is persisted at dispatch, so a missing row means capture is off (or the
    // row has not landed yet, a moment after dispatch).
    return (
      <div className={styles.traceNote}>
        {entry.pending
          ? 'No captured payload — capture is off for this key.'
          : 'No captured payload — capture is off, or this call predates it.'}
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

  // A request-only capture (persisted at dispatch) reads as an empty response
  // side: no body, no headers, no content type. While the call is in flight
  // that means "not yet", not "the vendor sent nothing" — say so.
  const resp = trace.data.response;
  const responsePending =
    entry.pending &&
    !resp.body &&
    !resp.content_type &&
    Object.keys(resp.headers).length === 0;

  return (
    <div className={styles.tracePanel}>
      <div className={styles.traceGrid}>
        <TraceSidePane title="Request" side={trace.data.request} />
        {responsePending ? (
          <div className={styles.traceSide}>
            <div className={styles.traceSideHead}>Response</div>
            <div className={styles.traceNote}>
              Response pending — the call is still in flight.
            </div>
          </div>
        ) : (
          <TraceSidePane title="Response" side={resp} />
        )}
      </div>
    </div>
  );
}

function TraceSidePane({ title, side }: { title: string; side: TraceSide }) {
  const headerEntries = Object.entries(side.headers);
  const display = displayBody(side.body, side.body_base64);
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
        {side.truncated && (
          <span
            className={`chip ${styles.truncatedChip}`}
            title="The encoded stream ended early; showing the recoverable decoded prefix."
          >
            partial decode
          </span>
        )}
        {/* Distinct from "partial decode": that says the capture is missing
            bytes, this says only the display is. Copy still has all of them. */}
        {display.elided > 0 && (
          <span
            className="chip"
            title="Long values are shortened here so the payload renders; Copy yields the untouched body."
          >
            {display.elided.toLocaleString()} chars elided
          </span>
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
          {display.text || <span className="muted">(empty)</span>}
        </pre>
      </div>
    </div>
  );
}
