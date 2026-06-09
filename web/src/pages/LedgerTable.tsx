import { useCallback, useMemo, useState } from 'react';
import { ChevronLeft, ChevronRight, Download, Search } from 'lucide-react';
import { api } from '../api/client';
import type { LedgerFilters, StatusGroup } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { StatusPill } from '../components/StatusPill';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { dateTime, money, ms } from '../lib/format';
import styles from './Overview.module.css';

const PAGE_SIZE = 25;
const REFRESH_MS = 10_000;

interface LedgerTableProps {
  since: number;
  until: number;
}

const STATUS_GROUPS: { value: StatusGroup; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'ok', label: 'OK' },
  { value: 'error', label: 'Errors' },
];

export function LedgerTable({ since, until }: LedgerTableProps) {
  const [model, setModel] = useState('');
  const [vendor, setVendor] = useState('');
  const [status, setStatus] = useState<StatusGroup>('all');
  const [offset, setOffset] = useState(0);
  const [exporting, setExporting] = useState(false);
  const toast = useToast();

  const filters: LedgerFilters = useMemo(
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
    () => api.ledger(filters),
    [since, until, model, vendor, status, offset],
    { intervalMs: REFRESH_MS },
  );

  const resetAndSet = useCallback((fn: () => void) => {
    setOffset(0);
    fn();
  }, []);

  const doExport = async (format: 'csv' | 'json') => {
    setExporting(true);
    try {
      await api.exportLedger(format, filters);
      toast.success(`Exported ledger as ${format.toUpperCase()}.`);
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
    <div className={`card ${styles.ledgerPanel}`}>
      <div className={styles.ledgerToolbar}>
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
                  <tr key={e.id}>
                    <td className="mono" style={{ color: 'var(--text-muted)' }}>
                      {dateTime(e.ts)}
                    </td>
                    <td className="mono">{e.model || '—'}</td>
                    <td>
                      <span className="chip" style={{ textTransform: 'capitalize' }}>
                        {e.modality || '—'}
                      </span>
                    </td>
                    <td>{e.vendor || '—'}</td>
                    <td className="num">{money(e.cost)}</td>
                    <td className="num">{ms(e.latency_ms)}</td>
                    <td>
                      <StatusPill status={e.status} />
                    </td>
                  </tr>
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
