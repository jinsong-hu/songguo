import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { ChevronLeft, ChevronRight } from 'lucide-react';
import { api } from '../api/client';
import type { CallsFilters, FeedRow } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useFetch, LIVE_REFRESH_MS } from '../lib/useFetch';
import { dateTime, duration, int, money } from '../lib/format';
import styles from './Overview.module.css';

const PAGE_SIZE = 25;
const REFRESH_MS = LIVE_REFRESH_MS;

interface ActivityFeedProps {
  since: number;
  until: number;
}

/**
 * ActivityFeed is the Overview's recent-activity table. Each row is either an
 * aggregated coding-agent session or a standalone request; clicking a row opens
 * the matching detail page. It replaces the old inline-expand calls table — the
 * captured trace now lives on the request page.
 */
export function ActivityFeed({ since, until }: ActivityFeedProps) {
  const [offset, setOffset] = useState(0);
  const navigate = useNavigate();

  const filters: CallsFilters = {
    since,
    until,
    limit: PAGE_SIZE,
    offset,
  };

  const { data, error, initialLoading, refetch } = useFetch(
    () => api.feed(filters),
    [since, until, offset],
    { intervalMs: REFRESH_MS },
  );

  const openRow = useCallback(
    (row: FeedRow) => {
      if (row.kind === 'session' && row.session_id) {
        navigate(`/sessions/${encodeURIComponent(row.session_id)}`);
      } else if (row.request_id) {
        navigate(`/calls/${encodeURIComponent(row.request_id)}`);
      }
    },
    [navigate],
  );

  const total = data?.total ?? 0;
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + PAGE_SIZE, total);
  const origin = window.location.origin;

  return (
    <div className={`card ${styles.callsPanel}`}>
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
      ) : !data || data.rows.length === 0 ? (
        <EmptyState
          title="No activity yet"
          hint={
            <>
              Point an SDK at <code>{origin}/v1</code> using a Songguo user key as the API
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
                  <th>Time</th>
                  <th>Session</th>
                  <th>Model</th>
                  <th className="num">Calls</th>
                  <th className="num">Tokens</th>
                  <th className="num">Cost</th>
                  <th className="num">Duration</th>
                </tr>
              </thead>
              <tbody>
                {data.rows.map((row) => (
                  <FeedRowView key={rowKey(row)} row={row} onOpen={() => openRow(row)} />
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
                onClick={() => setOffset((o) => Math.max(0, o - PAGE_SIZE))}
              >
                <ChevronLeft size={14} /> Prev
              </button>
              <button
                className="btn btn-sm"
                disabled={to >= total}
                onClick={() => setOffset((o) => o + PAGE_SIZE)}
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

function rowKey(row: FeedRow): string {
  return row.kind === 'session' ? `s:${row.session_id}` : `r:${row.request_id}`;
}

/** Short, readable form of a session id for the feed. */
function shortId(id: string): string {
  return id.length > 18 ? `${id.slice(0, 18)}…` : id;
}

function FeedRowView({ row, onOpen }: { row: FeedRow; onOpen: () => void }) {
  const isSession = row.kind === 'session';
  const tokens =
    row.input_tokens + row.cache_read_input_tokens + row.cache_creation_input_tokens + row.output_tokens;
  const model = isSession ? row.major_model || row.model || row.models[0] : row.model;

  return (
    <tr className={styles.callRow} style={{ cursor: 'pointer' }} onClick={onOpen}>
      <td className="mono" style={{ color: 'var(--text-muted)' }}>
        {dateTime(row.last_ts)}
      </td>
      <td>
        {isSession ? (
          <div className={styles.activitySession}>
            <span
              className={`${styles.activitySessionTitle} ${row.title ? '' : 'mono'}`}
              title={row.title || row.session_id}
            >
              {row.title || shortId(row.session_id ?? '')}
            </span>
          </div>
        ) : row.wire ? (
          <span className="mono" style={{ fontSize: 11.5, color: 'var(--text-muted)' }}>
            {row.wire}
          </span>
        ) : (
          <span style={{ color: 'var(--text-muted)' }}>—</span>
        )}
      </td>
      <td>
        {model ? (
          <span className="mono">{model}</span>
        ) : (
          <span style={{ color: 'var(--text-muted)' }}>—</span>
        )}
      </td>
      <td className="num">{row.calls}</td>
      <td className="num" title="input + output tokens">
        {tokens > 0 ? int(tokens) : '—'}
      </td>
      <td className="num">{money(row.cost)}</td>
      <td className="num">
        {row.duration_ms != null ? duration(row.duration_ms / 1000) : '—'}
      </td>
    </tr>
  );
}
