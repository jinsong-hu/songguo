import { useCallback, useMemo, useState } from 'react';
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Download,
  FileText,
  Search,
} from 'lucide-react';
import { api } from '../api/client';
import type { CallEntry, CallsFilters, CallTrace, StatusGroup, TraceSide } from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { StatusPill } from '../components/StatusPill';
import { useToast } from '../components/Toast';
import { useFetch, LIVE_REFRESH_MS } from '../lib/useFetch';
import { dateTime, money, ms } from '../lib/format';
import styles from './Overview.module.css';

const PAGE_SIZE = 25;
const REFRESH_MS = LIVE_REFRESH_MS;
/** Number of <td> in a call row — the expanded panel spans all of them. */
const COL_COUNT = 8;

interface CallsTableProps {
  since: number;
  until: number;
}

const STATUS_GROUPS: { value: StatusGroup; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'ok', label: 'OK' },
  { value: 'error', label: 'Errors' },
];

export function CallsTable({ since, until }: CallsTableProps) {
  const [model, setModel] = useState('');
  const [vendor, setVendor] = useState('');
  const [status, setStatus] = useState<StatusGroup>('all');
  const [offset, setOffset] = useState(0);
  const [exporting, setExporting] = useState(false);
  const [expanded, setExpanded] = useState<number | null>(null);
  const toast = useToast();

  const filters: CallsFilters = useMemo(
    () => ({
      since,
      until,
      model: model.trim() || undefined,
      vendor: vendor.trim() || undefined,
      status,
      limit: PAGE_SIZE,
      offset,
    }),
    [since, until, model, vendor, status, offset],
  );

  const { data, error, initialLoading, refetch } = useFetch(
    () => api.calls(filters),
    [since, until, model, vendor, status, offset],
    { intervalMs: REFRESH_MS },
  );

  const resetAndSet = useCallback((fn: () => void) => {
    setOffset(0);
    setExpanded(null);
    fn();
  }, []);

  const toggleRow = useCallback((id: number) => {
    setExpanded((cur) => (cur === id ? null : id));
  }, []);

  const doExport = async (format: 'csv' | 'json') => {
    setExporting(true);
    try {
      await api.exportCalls(format, filters);
      toast.success(`Exported calls as ${format.toUpperCase()}.`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Export failed.');
    } finally {
      setExporting(false);
    }
  };

  const total = data?.total ?? 0;
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + PAGE_SIZE, total);
  const origin = window.location.origin;

  return (
    <div className={`card ${styles.callsPanel}`}>
      <div className={styles.callsToolbar}>
        <div className={styles.search} style={{ position: 'relative' }}>
          <Search
            size={14}
            style={{
              position: 'absolute',
              left: 9,
              top: 9,
              color: 'var(--text-muted)',
              pointerEvents: 'none',
            }}
          />
          <input
            className="input"
            style={{ width: '100%', paddingLeft: 28 }}
            placeholder="Filter by model…"
            value={model}
            onChange={(e) => resetAndSet(() => setModel(e.target.value))}
          />
        </div>
        <input
          className="input"
          style={{ width: 160 }}
          placeholder="Filter by vendor…"
          value={vendor}
          onChange={(e) => resetAndSet(() => setVendor(e.target.value))}
        />
        <select
          className="select"
          value={status}
          onChange={(e) => resetAndSet(() => setStatus(e.target.value as StatusGroup))}
        >
          {STATUS_GROUPS.map((g) => (
            <option key={g.value} value={g.value}>
              {g.label}
            </option>
          ))}
        </select>
        <span className={styles.refreshDot} title="Auto-refreshing every 10s">
          <span className={styles.live} />
          Live
        </span>
        <div className={styles.spacer} />
        <button
          className="btn btn-sm"
          onClick={() => doExport('csv')}
          disabled={exporting}
        >
          <Download size={13} /> CSV
        </button>
        <button
          className="btn btn-sm"
          onClick={() => doExport('json')}
          disabled={exporting}
        >
          <Download size={13} /> JSON
        </button>
      </div>

      {error ? (
        <div style={{ padding: 16 }}>
          <ErrorBanner message={error} onRetry={refetch} />
        </div>
      ) : initialLoading ? (
        <div style={{ padding: 16, display: 'flex', flexDirection: 'column', gap: 10 }}>
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} height={20} />
          ))}
        </div>
      ) : !data || data.entries.length === 0 ? (
        <EmptyState
          title="No calls yet"
          hint={
            <>
              Point an SDK at <code>{origin}/v1</code> using a Songguo token as the API
              key to start logging usage.
            </>
          }
        />
      ) : (
        <>
          <div className={styles.tableScroll}>
            <table className="table">
              <thead>
                <tr>
                  <th className={styles.expandCol} aria-label="Expand" />
                  <th>Time</th>
                  <th>Model</th>
                  <th>Modality</th>
                  <th>Vendor</th>
                  <th className="num">Cost</th>
                  <th className="num">Latency</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {data.entries.map((e) => (
                  <CallRow
                    key={e.id}
                    entry={e}
                    open={expanded === e.id}
                    onToggle={() => toggleRow(e.id)}
                  />
                ))}
              </tbody>
            </table>
          </div>
          <div className={styles.pager}>
            <span>
              {from}–{to} of {total.toLocaleString('en-US')}
            </span>
            <div className={styles.pagerBtns}>
              <button
                className="btn btn-sm"
                disabled={offset === 0}
                onClick={() => {
                  setExpanded(null);
                  setOffset((o) => Math.max(0, o - PAGE_SIZE));
                }}
              >
                <ChevronLeft size={14} /> Prev
              </button>
              <button
                className="btn btn-sm"
                disabled={to >= total}
                onClick={() => {
                  setExpanded(null);
                  setOffset((o) => o + PAGE_SIZE);
                }}
              >
                Next <ChevronRight size={14} />
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}

interface CallRowProps {
  entry: CallEntry;
  open: boolean;
  onToggle: () => void;
}

function CallRow({ entry, open, onToggle }: CallRowProps) {
  return (
    <>
      <tr
        className={`${styles.callRow} ${open ? styles.callRowOpen : ''}`}
        onClick={onToggle}
      >
        <td className={styles.expandCol}>
          <button
            type="button"
            className={styles.expandBtn}
            aria-label={open ? 'Collapse row' : 'Expand row'}
            aria-expanded={open}
            onClick={(ev) => {
              ev.stopPropagation();
              onToggle();
            }}
          >
            <ChevronDown
              size={14}
              className={`${styles.chevron} ${open ? styles.chevronOpen : ''}`}
            />
          </button>
        </td>
        <td className="mono" style={{ color: 'var(--text-muted)' }}>
          <span className={styles.timeCell}>
            {dateTime(entry.ts)}
            {entry.has_trace && (
              <FileText
                size={12}
                className={styles.traceIcon}
                aria-label="Captured payload available"
              />
            )}
          </span>
        </td>
        <td className="mono">{entry.model || '—'}</td>
        <td>
          <span className="chip" style={{ textTransform: 'capitalize' }}>
            {entry.modality || '—'}
          </span>
        </td>
        <td>{entry.vendor || '—'}</td>
        <td className="num">{money(entry.cost)}</td>
        <td className="num">{ms(entry.latency_ms)}</td>
        <td>
          <StatusPill status={entry.status} />
        </td>
      </tr>
      {open && (
        <tr className={styles.traceRow}>
          <td colSpan={COL_COUNT} className={styles.traceCell}>
            <TracePanel entry={entry} />
          </td>
        </tr>
      )}
    </>
  );
}

function TracePanel({ entry }: { entry: CallEntry }) {
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
        {side.truncated && (
          <span className={`chip ${styles.truncatedChip}`}>truncated</span>
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
