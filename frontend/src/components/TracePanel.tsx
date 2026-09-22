import { useState } from 'react';
import { ChevronRight } from 'lucide-react';
import { api } from '../api/client';
import type { CallEntry, CallTrace, TraceSide } from '../api/types';
import { CopyButton } from './CopyButton';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import { displayBody } from '../lib/traceBody';
import { useFetch } from '../lib/useFetch';
import styles from '../pages/ActivityFeed.module.css';

/**
 * TracePanel renders the captured request/response payload for a call, behind a
 * disclosure that fetches nothing until it is opened. Shows a note when no
 * payload was captured.
 *
 * The disclosure is the point, not decoration. This is the largest read the
 * dashboard makes — both BLOB columns of one `raw` row — and a captured agent
 * turn is thousands of overflow pages that SQLite walks one dependent read at a
 * time, on a disk the rest of the box shares. It used to fire on page load,
 * concurrently with the /messages fetch beside it, so every open of a call
 * detail page read the same multi-MB row twice before anyone had asked to see
 * the bytes.
 *
 * What makes closed-by-default the right default rather than a tax: the
 * PromptReconstruction card above already renders both halves of the turn
 * formatted — what was sent and what came back. The raw JSON here is the
 * fallback for when that rendering is not enough, so it is worth one click.
 */
export function TracePanel({ entry }: { entry: CallEntry }) {
  const [open, setOpen] = useState(false);
  const trace = useFetch<CallTrace>((signal) => api.trace(entry.id, signal), [entry.id], {
    enabled: entry.has_trace && open,
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

  return (
    <details
      className={styles.traceDisclosure}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary className={styles.traceDisclosureHead}>
        <ChevronRight size={15} className={styles.traceChevron} />
        <span>Raw request and response</span>
        <span className={styles.traceDisclosureHint}>reads the stored body</span>
      </summary>
      <TraceBody entry={entry} trace={trace} />
    </details>
  );
}

/**
 * TraceBody is the opened disclosure's contents. It is a separate component so
 * the fetch states render inside the <details> rather than in place of it —
 * otherwise an error or a slow load would replace the summary the reader just
 * clicked, and there would be nothing left to close.
 */
function TraceBody({
  entry,
  trace,
}: {
  entry: CallEntry;
  trace: ReturnType<typeof useFetch<CallTrace>>;
}) {
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
