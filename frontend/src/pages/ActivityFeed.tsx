import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { ChevronDown, ChevronLeft, ChevronRight } from 'lucide-react';
import { api } from '../api/client';
import type { CallsFilters, FeedRow, FeedSort } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useFetch, LIVE_REFRESH_MS } from '../lib/useFetch';
import { dateTime, duration, int, money } from '../lib/format';
import styles from './Overview.module.css';

const PAGE_SIZE = 25;
const REFRESH_MS = LIVE_REFRESH_MS;

// The "Top ▾" dropdown picks a ranking metric; these are its options.
const TOP_SORTS: { key: FeedSort; label: string }[] = [
  { key: 'tokens', label: 'Tokens' },
  { key: 'cost', label: 'Cost' },
  { key: 'duration', label: 'Duration' },
  { key: 'turns', label: 'Turns' },
];
const TOP_KEYS = TOP_SORTS.map((t) => t.key);

interface ActivityFeedProps {
  since: number;
  until: number;
}

/**
 * ActivityFeed is the Overview's recent-activity table. Each row is either an
 * aggregated coding-agent session or a standalone request; clicking a row opens
 * the matching detail page. It replaces the old inline-expand calls table — the
 * captured trace now lives on the request page.
 *
 * The title row carries sort tabs — Recent (default) · Top ▾ · Slow · Failures —
 * that re-rank the whole window server-side (see FeedSort). Changing the sort
 * resets to the first page.
 */
export function ActivityFeed({ since, until }: ActivityFeedProps) {
  const [offset, setOffset] = useState(0);
  const [sort, setSort] = useState<FeedSort>('recent');
  const navigate = useNavigate();

  // Changing the sort must restart pagination, else offset can point past the
  // end of the newly-ranked list.
  const changeSort = useCallback((next: FeedSort) => {
    setSort(next);
    setOffset(0);
  }, []);

  const filters: CallsFilters = {
    since,
    until,
    sort,
    limit: PAGE_SIZE,
    offset,
  };

  const { data, error, initialLoading, refetch } = useFetch(
    () => api.feed(filters),
    [since, until, sort, offset],
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
    <>
      <div className={styles.sectionTitle}>
        <span className={styles.sectionName}>Recent activity</span>
        <FeedTabs sort={sort} onChange={changeSort} />
      </div>
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
        sort === 'failures' ? (
          <EmptyState title="No failures in this range" hint="Every request in this window succeeded." />
        ) : (
          <EmptyState
            title="No activity yet"
            hint={
              <>
                Point an SDK at <code>{origin}/v1</code> using a Songguo user key as the API
                key to start logging usage.
              </>
            }
          />
        )
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
    </>
  );
}

/**
 * FeedTabs is the sort control on the Recent-activity title row: a segmented
 * bar of Recent · Top ▾ · Slow · Failures. Recent/Slow/Failures set the sort
 * directly; the Top segment is a native <select> overlaid transparently on the
 * button (zero deps, keyboard-accessible) that picks the ranking metric — it
 * reads active when any of the four top* sorts is selected.
 */
function FeedTabs({ sort, onChange }: { sort: FeedSort; onChange: (s: FeedSort) => void }) {
  const topActive = TOP_KEYS.includes(sort);
  const topLabel = TOP_SORTS.find((t) => t.key === sort)?.label ?? 'Tokens';
  // The metric the dropdown shows/uses when Top isn't the active tab yet: keep
  // the last-picked top metric, else default to Tokens.
  const topValue: FeedSort = topActive ? sort : 'tokens';

  return (
    <div className={styles.seg} role="tablist" aria-label="Activity sort">
      <button
        role="tab"
        aria-selected={sort === 'recent'}
        className={`${styles.segBtn} ${sort === 'recent' ? styles.segActive : ''}`}
        onClick={() => onChange('recent')}
      >
        Recent
      </button>

      <div className={`${styles.segBtn} ${styles.segSelect} ${topActive ? styles.segActive : ''}`}>
        <span>Top · {topLabel}</span>
        <ChevronDown size={13} aria-hidden="true" />
        <select
          className={styles.segSelectInput}
          aria-label="Top ranking metric"
          value={topValue}
          onChange={(e) => onChange(e.target.value as FeedSort)}
        >
          {TOP_SORTS.map((t) => (
            <option key={t.key} value={t.key}>
              Top {t.label.toLowerCase()}
            </option>
          ))}
        </select>
      </div>

      <button
        role="tab"
        aria-selected={sort === 'slow'}
        className={`${styles.segBtn} ${sort === 'slow' ? styles.segActive : ''}`}
        onClick={() => onChange('slow')}
      >
        Slow
      </button>
      <button
        role="tab"
        aria-selected={sort === 'failures'}
        className={`${styles.segBtn} ${sort === 'failures' ? styles.segActive : ''}`}
        onClick={() => onChange('failures')}
      >
        Failures
      </button>
    </div>
  );
}

function rowKey(row: FeedRow): string {
  return row.kind === 'session' ? `s:${row.session_id}` : `r:${row.request_id}`;
}

/**
 * The usage column is unit-aware: speech wires meter no tokens, so ASR bills by
 * audio duration (seconds) and TTS by text length (characters). Prefer whichever
 * unit the row actually accrued; fall back to tokens, then em-dash when nothing
 * was metered.
 */
function usageCell(row: FeedRow, tokens: number): string {
  if (row.seconds > 0) return duration(row.seconds);
  if (row.chars > 0) return `${int(row.chars)} ch`;
  return tokens > 0 ? int(tokens) : '—';
}

function usageTip(row: FeedRow): string {
  if (row.seconds > 0) return 'billed audio duration';
  if (row.chars > 0) return 'billed characters';
  return 'input + output tokens';
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
      <td className="num" title={usageTip(row)}>
        {usageCell(row, tokens)}
      </td>
      <td className="num">{money(row.cost)}</td>
      <td className="num">
        {row.duration_ms != null ? duration(row.duration_ms / 1000) : '—'}
      </td>
    </tr>
  );
}
