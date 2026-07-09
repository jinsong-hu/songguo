import { Info, Lock, LockOpen, LogOut, Moon, Sun } from 'lucide-react';
import { api } from '../api/client';
import { CopyButton } from '../components/CopyButton';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { useSettings } from '../lib/settingsContext';
import { useTheme } from '../lib/useTheme';
import styles from './SettingsPage.module.css';

export function SettingsPage() {
  const { settings, signOut } = useSettings();
  const { theme, setTheme } = useTheme();
  const pricing = useFetch(() => api.pricing(), []);

  const consumerUrl = `${window.location.origin}/v1`;

  return (
    <Page
      title="Settings"
      actions={
        <button className="btn btn-danger" onClick={signOut}>
          <LogOut size={14} /> Sign out
        </button>
      }
    >
      <div className={styles.sections}>
        {/* Appearance */}
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Appearance</div>
          <div className={styles.panelDesc}>Choose how the dashboard looks.</div>
          <div className={styles.themeRow}>
            <button
              className={`${styles.themeBtn} ${theme === 'light' ? styles.themeActive : ''}`}
              onClick={() => setTheme('light')}
            >
              <Sun size={15} /> Light
            </button>
            <button
              className={`${styles.themeBtn} ${theme === 'dark' ? styles.themeActive : ''}`}
              onClick={() => setTheme('dark')}
            >
              <Moon size={15} /> Dark
            </button>
          </div>
        </div>

        {/* Connection */}
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Connection</div>
          <div className={styles.panelDesc}>
            Point your SDK&apos;s base URL here and use a Songguo user key as the API key.
          </div>
          <div className={styles.connRow}>
            <span className={styles.connLabel}>Consumer base URL</span>
            <div className={styles.connField}>
              <code className={styles.connValue}>{consumerUrl}</code>
              <CopyButton value={consumerUrl} label="Copy" />
            </div>
          </div>
          <div className={styles.connHint}>
            For example, set <code>OPENAI_BASE_URL={consumerUrl}</code> and
            <code> OPENAI_API_KEY=&lt;your-songguo-key&gt;</code>. Requests are proxied
            transparently to the routed vendor.
          </div>
        </div>

        {/* Admin / runtime */}
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Runtime</div>
          <div className={styles.panelDesc}>Read-only server configuration.</div>
          <div className={styles.meta}>
            <span className={styles.metaKey}>Admin API</span>
            <span>
              <span
                className={`${styles.statusBadge} ${
                  settings.admin_protected ? styles.statusProtected : styles.statusOpen
                }`}
              >
                {settings.admin_protected ? <Lock size={11} /> : <LockOpen size={11} />}
                {settings.admin_protected ? 'Protected' : 'Unprotected'}
              </span>
            </span>

            <span className={styles.metaKey}>Version</span>
            <span className={styles.metaVal}>{settings.version}</span>

            <span className={styles.metaKey}>Listen</span>
            <span className={styles.metaVal}>{settings.listen || '—'}</span>

            <span className={styles.metaKey}>Database path</span>
            <span className={styles.metaVal}>{settings.db_path || '—'}</span>
          </div>
        </div>

        {/* Pricing */}
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelTitle}>Pricing</div>
          <div className={styles.panelDesc}>
            Per-model rates used to compute usage costs.
          </div>
          {pricing.error ? (
            <ErrorBanner message={pricing.error} onRetry={pricing.refetch} />
          ) : pricing.initialLoading ? (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} height={20} />
              ))}
            </div>
          ) : !pricing.data || pricing.data.length === 0 ? (
            <span className="muted" style={{ fontSize: 13 }}>
              No pricing configured.
            </span>
          ) : (
            <div className={styles.tableScroll}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Vendor</th>
                    <th>Model</th>
                    <th className="num">Input</th>
                    <th className="num">Output</th>
                    <th>Unit</th>
                  </tr>
                </thead>
                <tbody>
                  {pricing.data.map((row) => (
                    <tr key={`${row.vendor}:${row.model}`}>
                      <td>{row.vendor}</td>
                      <td className="mono">{row.model}</td>
                      <td className="num">{row.input}</td>
                      <td className="num">{row.output}</td>
                      <td className="mono">{row.unit}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className={styles.hint}>
            <Info size={14} />
            Pricing is configured per provider and stored in the database — edit a
            provider to change its rates.
          </div>
        </div>
      </div>
    </Page>
  );
}
