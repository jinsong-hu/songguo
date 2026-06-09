import { useState } from 'react';
import { Activity, CheckCircle2, Info, XCircle } from 'lucide-react';
import { api } from '../api/client';
import type { Vendor, VendorTestResult } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { ms, percent } from '../lib/format';
import styles from './Vendors.module.css';

export function VendorsPage() {
  const { data, error, initialLoading, refetch } = useFetch(() => api.vendors(), []);

  return (
    <Page title="Vendors">
      <div className={styles.hint}>
        <Info size={15} />
        Vendors are defined in <code>config.yaml</code> and hot-reloaded — edit the file
        to add or change them.
      </div>

      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading ? (
        <div className={styles.list}>
          {Array.from({ length: 2 }).map((_, i) => (
            <div key={i} className={`card ${styles.vendor}`} style={{ padding: 16 }}>
              <Skeleton height={20} width={180} />
              <Skeleton height={14} width="60%" style={{ marginTop: 10 }} />
            </div>
          ))}
        </div>
      ) : !data || data.length === 0 ? (
        <EmptyState
          icon={Activity}
          title="No vendors configured"
          hint={
            <>
              Add a vendor block to <code>config.yaml</code>; Songguo hot-reloads it
              automatically.
            </>
          }
        />
      ) : (
        <div className={styles.list}>
          {data.map((v) => (
            <VendorCard key={v.name} vendor={v} />
          ))}
        </div>
      )}
    </Page>
  );
}

function VendorCard({ vendor }: { vendor: Vendor }) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<VendorTestResult | null>(null);

  const runTest = async () => {
    setTesting(true);
    setResult(null);
    try {
      setResult(await api.testVendor(vendor.name));
    } catch (e) {
      setResult({
        reachable: false,
        status: 0,
        latency_ms: 0,
        error: e instanceof Error ? e.message : 'Test failed',
      });
    } finally {
      setTesting(false);
    }
  };

  const { stats } = vendor;
  const priceModels = Object.entries(vendor.prices).sort((a, b) =>
    a[0].localeCompare(b[0]),
  );

  return (
    <div className={`card ${styles.vendor}`}>
      <div className={styles.head}>
        <div className={styles.headLeft}>
          <div className={styles.nameRow}>
            <span className={styles.name}>{vendor.name}</span>
            <span
              className={`${styles.badge} ${stats.healthy ? styles.healthy : styles.unhealthy}`}
            >
              {stats.healthy ? <CheckCircle2 size={12} /> : <XCircle size={12} />}
              {stats.healthy ? 'Healthy' : 'Unhealthy'}
            </span>
          </div>
          <div className={styles.baseUrl}>{vendor.base_url}</div>
        </div>
        <div className={styles.headRight}>
          <button className="btn btn-sm" onClick={runTest} disabled={testing}>
            {testing ? <span className="spinner" style={{ width: 13, height: 13 }} /> : null}
            {testing ? 'Testing…' : 'Test'}
          </button>
          {result && (
            <div
              className={`${styles.testResult} ${
                result.reachable ? styles.testOk : styles.testErr
              }`}
            >
              {result.reachable
                ? `reachable · ${result.status} · ${ms(result.latency_ms)}`
                : result.error || `unreachable · ${result.status}`}
            </div>
          )}
        </div>
      </div>

      <div className={styles.body}>
        <div className={styles.section}>
          <span className={styles.sectionTitle}>Served models</span>
          <div className={styles.chips}>
            {vendor.served_models.length === 0 ? (
              <span className="muted" style={{ fontSize: 12 }}>
                None
              </span>
            ) : (
              vendor.served_models.map((m) => (
                <span key={m} className="chip chip-mono">
                  {m}
                </span>
              ))
            )}
          </div>

          <div className={styles.meta} style={{ marginTop: 8 }}>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Priority</span>
              <span className={styles.metaValue}>{vendor.priority}</span>
            </div>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Weight</span>
              <span className={styles.metaValue}>{vendor.weight}</span>
            </div>
          </div>

          <span className={styles.sectionTitle} style={{ marginTop: 8 }}>
            Credentials
          </span>
          <div className={styles.creds}>
            {vendor.credentials.length === 0 ? (
              <span className="muted" style={{ fontSize: 12 }}>
                None
              </span>
            ) : (
              vendor.credentials.map((c) => (
                <span key={c.id} className={styles.cred}>
                  {c.id}
                  <span className={styles.arrow}>→</span>
                  {c.masked_key}
                </span>
              ))
            )}
          </div>
        </div>

        <div className={styles.section}>
          <span className={styles.sectionTitle}>Stats</span>
          <div className={styles.meta}>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Requests</span>
              <span className={styles.metaValue}>{stats.requests}</span>
            </div>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Error rate</span>
              <span className={styles.metaValue}>{percent(stats.error_rate)}</span>
            </div>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Avg latency</span>
              <span className={styles.metaValue}>{ms(stats.avg_latency_ms)}</span>
            </div>
          </div>

          <span className={styles.sectionTitle} style={{ marginTop: 8 }}>
            Prices
          </span>
          {priceModels.length === 0 ? (
            <span className="muted" style={{ fontSize: 12 }}>
              No prices configured
            </span>
          ) : (
            <table className={styles.priceTable}>
              <thead>
                <tr>
                  <th>Model</th>
                  <th style={{ textAlign: 'right' }}>Input</th>
                  <th style={{ textAlign: 'right' }}>Output</th>
                  <th>Unit</th>
                </tr>
              </thead>
              <tbody>
                {priceModels.map(([model, p]) => (
                  <tr key={model}>
                    <td className="mono">{model}</td>
                    <td className="n">{p.input}</td>
                    <td className="n">{p.output}</td>
                    <td className="mono">{p.unit}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}
