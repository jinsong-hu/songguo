import { useState, type FormEvent } from 'react';
import {
  Eye,
  EyeOff,
  Info,
  Lock,
  LockOpen,
  LogOut,
  Monitor,
  Moon,
  Pencil,
  Plus,
  Sun,
  Trash2,
} from 'lucide-react';
import { api } from '../api/client';
import type {
  CreateProxyBody,
  PatchProxyBody,
  Proxy as OutboundProxy,
  ProxyType,
} from '../api/types';
import { CopyButton } from '../components/CopyButton';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { useSettings } from '../lib/settingsContext';
import { useTheme } from '../lib/useTheme';
import styles from './SettingsPage.module.css';

export function SettingsPage() {
  const { settings, signOut } = useSettings();
  const { theme, setTheme } = useTheme();
  const pricing = useFetch(() => api.pricing(), []);
  const proxies = useFetch(() => api.proxies(), []);
  const toast = useToast();
  const [editingProxy, setEditingProxy] = useState<OutboundProxy | 'new' | null>(null);
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);

  const consumerUrl = `${window.location.origin}/v1`;

  const deleteProxy = async (proxy: OutboundProxy) => {
    setDeleteBusy(true);
    try {
      await api.deleteProxy(proxy.id);
      setEditingProxy((current) =>
        current !== 'new' && current?.id === proxy.id ? null : current,
      );
      proxies.refetch();
      toast.success(`Deleted "${proxy.name}".`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Delete failed.');
    } finally {
      setConfirmingDelete(null);
      setDeleteBusy(false);
    }
  };

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
              className={`${styles.themeBtn} ${theme === 'auto' ? styles.themeActive : ''}`}
              onClick={() => setTheme('auto')}
            >
              <Monitor size={15} /> Auto
            </button>
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

        {/* Outbound proxies */}
        <div className={`card ${styles.panel}`}>
          <div className={styles.panelHead}>
            <div>
              <div className={styles.panelTitle}>Proxies</div>
              <div className={styles.panelDesc}>Reusable outbound routes for providers.</div>
            </div>
            <button className="btn btn-primary" onClick={() => setEditingProxy('new')}>
              <Plus size={14} /> Add proxy
            </button>
          </div>

          {editingProxy && (
            <ProxyForm
              key={editingProxy === 'new' ? 'new' : editingProxy.id}
              proxy={editingProxy === 'new' ? undefined : editingProxy}
              onCancel={() => setEditingProxy(null)}
              onSaved={(saved) => {
                setEditingProxy(null);
                proxies.refetch();
                toast.success(`Saved "${saved.name}".`);
              }}
            />
          )}

          {proxies.error ? (
            <ErrorBanner message={proxies.error} onRetry={proxies.refetch} />
          ) : proxies.initialLoading ? (
            <div className={styles.proxySkeletons}>
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} height={28} />
              ))}
            </div>
          ) : !proxies.data || proxies.data.length === 0 ? (
            <div className={styles.proxyEmpty}>No proxies configured.</div>
          ) : (
            <div className={styles.tableScroll}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Route</th>
                    <th>Auth</th>
                    <th className="num">Providers</th>
                    <th style={{ textAlign: 'right' }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {proxies.data.map((proxy) => (
                    <tr key={proxy.id}>
                      <td>{proxy.name}</td>
                      <td className="mono">
                        {proxy.type.toUpperCase()} · {proxy.host}:{proxy.port}
                      </td>
                      <td>{proxy.username ? proxy.username : <span className="muted">None</span>}</td>
                      <td className="num">{proxy.provider_count}</td>
                      <td>
                        <div className={styles.proxyActions}>
                          {confirmingDelete === proxy.id ? (
                            <>
                              <button
                                className="btn btn-sm btn-danger"
                                disabled={deleteBusy}
                                onClick={() => void deleteProxy(proxy)}
                              >
                                {deleteBusy ? 'Deleting…' : 'Confirm delete'}
                              </button>
                              <button
                                className="btn btn-sm"
                                disabled={deleteBusy}
                                onClick={() => setConfirmingDelete(null)}
                              >
                                Cancel
                              </button>
                            </>
                          ) : (
                            <>
                              <button
                                className="btn btn-sm"
                                onClick={() => setEditingProxy(proxy)}
                              >
                                <Pencil size={12} /> Edit
                              </button>
                              <button
                                className="btn btn-sm btn-danger"
                                disabled={proxy.provider_count > 0}
                                title={
                                  proxy.provider_count > 0
                                    ? 'Set assigned providers to Direct before deleting this proxy.'
                                    : 'Delete proxy'
                                }
                                onClick={() => setConfirmingDelete(proxy.id)}
                              >
                                <Trash2 size={12} /> Delete
                              </button>
                            </>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
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

function ProxyForm({
  proxy,
  onCancel,
  onSaved,
}: {
  proxy?: OutboundProxy;
  onCancel: () => void;
  onSaved: (proxy: OutboundProxy) => void;
}) {
  const [name, setName] = useState(proxy?.name ?? '');
  const [type, setType] = useState<ProxyType>(proxy?.type ?? 'https');
  const [host, setHost] = useState(proxy?.host ?? '');
  const [port, setPort] = useState(String(proxy?.port ?? 443));
  const [username, setUsername] = useState(proxy?.username ?? '');
  const [password, setPassword] = useState('');
  const [clearPassword, setClearPassword] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const changeType = (next: ProxyType) => {
    const oldDefault = type === 'https' ? '443' : '1080';
    setType(next);
    if (port === '' || port === oldDefault) setPort(next === 'https' ? '443' : '1080');
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (busy) return;

    const parsedPort = Number(port);
    if (!name.trim() || !host.trim()) {
      setError('Name and host are required.');
      return;
    }
    if (!Number.isInteger(parsedPort) || parsedPort < 1 || parsedPort > 65535) {
      setError('Port must be an integer between 1 and 65535.');
      return;
    }
    if (password && !username) {
      setError('Enter a username when setting a password.');
      return;
    }

    setBusy(true);
    setError(null);
    try {
      let saved: OutboundProxy;
      if (proxy) {
        const body: PatchProxyBody = {
          name: name.trim(),
          type,
          host: host.trim(),
          port: parsedPort,
          username,
        };
        if (password) body.password = password;
        if (clearPassword || (!username && proxy.has_password)) body.clear_password = true;
        saved = await api.patchProxy(proxy.id, body);
      } else {
        const body: CreateProxyBody = {
          name: name.trim(),
          type,
          host: host.trim(),
          port: parsedPort,
          username,
          password: password || undefined,
        };
        saved = await api.createProxy(body);
      }
      onSaved(saved);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <form className={styles.proxyEditor} onSubmit={submit}>
      <div className={styles.proxyFormGrid}>
        <label className={styles.proxyField}>
          <span className={styles.proxyLabel}>Name</span>
          <input
            className="input"
            value={name}
            autoFocus
            placeholder="e.g. office-egress"
            onChange={(e) => setName(e.target.value)}
          />
        </label>

        <div className={styles.proxyField}>
          <span className={styles.proxyLabel}>Protocol</span>
          <div className={styles.protocolSwitch}>
            {(['https', 'socks5'] as const).map((value) => (
              <button
                key={value}
                type="button"
                className={type === value ? styles.protocolActive : ''}
                aria-pressed={type === value}
                onClick={() => changeType(value)}
              >
                {value.toUpperCase()}
              </button>
            ))}
          </div>
        </div>

        <label className={`${styles.proxyField} ${styles.proxyHost}`}>
          <span className={styles.proxyLabel}>Host</span>
          <input
            className="input mono"
            value={host}
            placeholder="proxy.example.com"
            onChange={(e) => setHost(e.target.value)}
          />
        </label>

        <label className={styles.proxyField}>
          <span className={styles.proxyLabel}>Port</span>
          <input
            className="input mono"
            inputMode="numeric"
            value={port}
            onChange={(e) => setPort(e.target.value)}
          />
        </label>

        <label className={styles.proxyField}>
          <span className={styles.proxyLabel}>Username</span>
          <input
            className="input"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </label>

        <label className={styles.proxyField}>
          <span className={styles.proxyLabel}>Password</span>
          <span className={styles.passwordField}>
            <input
              className="input"
              type={showPassword ? 'text' : 'password'}
              autoComplete="new-password"
              value={password}
              placeholder={proxy?.has_password ? 'Leave blank to keep' : ''}
              onChange={(e) => {
                setPassword(e.target.value);
                if (e.target.value) setClearPassword(false);
              }}
            />
            <button
              type="button"
              className={styles.passwordToggle}
              aria-label={showPassword ? 'Hide password' : 'Show password'}
              onClick={() => setShowPassword((shown) => !shown)}
            >
              {showPassword ? <EyeOff size={14} /> : <Eye size={14} />}
            </button>
          </span>
        </label>
      </div>

      {proxy?.has_password && (
        <label className={styles.clearAuth}>
          <input
            type="checkbox"
            checked={clearPassword}
            onChange={(e) => {
              setClearPassword(e.target.checked);
              if (e.target.checked) setPassword('');
            }}
          />
          Clear saved password
        </label>
      )}

      {error && <div className={styles.proxyError}>{error}</div>}

      <div className={styles.proxyFormActions}>
        <button type="button" className="btn" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? 'Saving…' : proxy ? 'Save changes' : 'Add proxy'}
        </button>
      </div>
    </form>
  );
}
