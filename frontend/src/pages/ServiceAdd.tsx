import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import {
  ArrowLeft,
  Check,
  CheckCircle2,
  KeyRound,
  Pencil,
  Plus,
  Search,
  Trash2,
  Wrench,
  XCircle,
} from 'lucide-react';
import { api } from '../api/client';
import type { CatalogService, CatalogVendor, Provider, VendorTestResult } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { ms, percent } from '../lib/format';
import styles from './ServiceAdd.module.css';

interface FlatEntry {
  vendor: CatalogVendor;
  service: CatalogService;
}

export function ServiceAddPage() {
  const catalog = useFetch(() => api.catalog(), []);
  const providers = useFetch(() => api.providers(), []);
  const [query, setQuery] = useState('');
  const [vendorFilter, setVendorFilter] = useState<string>('all');
  const navigate = useNavigate();
  const toast = useToast();

  const onDelete = async (provider: Provider) => {
    try {
      await api.deleteProvider(provider.id);
      providers.refetch();
      toast.success(`Removed "${provider.name}".`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Delete failed.');
    }
  };

  const addedCatalogIds = useMemo(() => {
    const set = new Set<string>();
    for (const s of providers.data ?? []) if (s.catalog_id) set.add(s.catalog_id);
    return set;
  }, [providers.data]);

  const entries = useMemo<FlatEntry[]>(() => {
    const out: FlatEntry[] = [];
    for (const v of catalog.data?.vendors ?? []) {
      for (const s of v.services) out.push({ vendor: v, service: s });
    }
    return out;
  }, [catalog.data]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return entries.filter(({ vendor, service }) => {
      if (vendorFilter !== 'all' && vendor.id !== vendorFilter) return false;
      if (!q) return true;
      if (vendor.name.toLowerCase().includes(q)) return true;
      if (service.name.toLowerCase().includes(q)) return true;
      if (service.kind.toLowerCase().includes(q)) return true;
      return service.models.some((m) => m.model.toLowerCase().includes(q));
    });
  }, [entries, query, vendorFilter]);

  const openAdd = (service: CatalogService) => {
    navigate(`/providers/new?preset=${encodeURIComponent(service.id)}`);
  };

  const hasProviders = (providers.data?.length ?? 0) > 0;

  return (
    <Page
      title="Add service"
      actions={
        <Link to="/services" className="btn">
          <ArrowLeft size={15} /> Back to services
        </Link>
      }
    >
      {providers.error ? (
        <ErrorBanner message={providers.error} onRetry={providers.refetch} />
      ) : providers.initialLoading ? (
        <section className={styles.sectionBlock}>
          <h2 className={styles.sectionHeading}>Configured providers</h2>
          <div className={styles.list}>
            {Array.from({ length: 2 }).map((_, i) => (
              <div key={i} className={`card ${styles.provider}`} style={{ padding: 16 }}>
                <Skeleton height={20} width={180} />
                <Skeleton height={14} width="60%" style={{ marginTop: 10 }} />
              </div>
            ))}
          </div>
        </section>
      ) : hasProviders ? (
        <section className={styles.sectionBlock}>
          <h2 className={styles.sectionHeading}>Configured providers</h2>
          <div className={styles.list}>
            {(providers.data ?? []).map((p) => (
              <ProviderCard
                key={p.id}
                provider={p}
                onChanged={providers.refetch}
                onEdit={() => navigate(`/providers/${p.id}/edit`)}
                onDelete={() => onDelete(p)}
              />
            ))}
          </div>
        </section>
      ) : null}

      <h2 className={styles.sectionHeading}>{hasProviders ? 'Add another provider' : 'Add a provider'}</h2>
      <div className={styles.intro}>
        Pick a preset — endpoint, wires, models, and prices come pre-filled, you just paste your
        API key — or configure a custom provider from scratch.
      </div>

      {catalog.error ? (
        <ErrorBanner message={catalog.error} onRetry={catalog.refetch} />
      ) : catalog.initialLoading ? (
        <div className={styles.grid}>
          {Array.from({ length: 4 }).map((_, i) => (
            <div key={i} className={`card ${styles.entry}`} style={{ padding: 16 }}>
              <Skeleton height={18} width={160} />
              <Skeleton height={13} width="70%" style={{ marginTop: 10 }} />
            </div>
          ))}
        </div>
      ) : (
        <>
          <div className={styles.controls}>
            <div className={styles.searchBox}>
              <Search size={15} />
              <input
                className={styles.searchInput}
                value={query}
                placeholder="Search presets, models, providers…"
                onChange={(e) => setQuery(e.target.value)}
              />
            </div>
            <div className={styles.facets}>
              <button
                className={`${styles.facet} ${vendorFilter === 'all' ? styles.facetActive : ''}`}
                onClick={() => setVendorFilter('all')}
              >
                All
              </button>
              {(catalog.data?.vendors ?? []).map((v) => (
                <button
                  key={v.id}
                  className={`${styles.facet} ${vendorFilter === v.id ? styles.facetActive : ''}`}
                  onClick={() => setVendorFilter(v.id)}
                >
                  {v.name}
                </button>
              ))}
            </div>
          </div>

          {filtered.length === 0 && query.trim() !== '' ? (
            <EmptyState icon={Search} title="No matches" hint="Try a different search or facet." />
          ) : (
            <div className={styles.grid}>
              <button
                className={`card ${styles.entry} ${styles.custom}`}
                onClick={() => navigate('/providers/new')}
              >
                <div className={styles.customIcon}>
                  <Wrench size={18} />
                </div>
                <span className={styles.serviceName}>Custom provider</span>
                <span className={styles.note}>
                  Any OpenAI- or Anthropic-compatible endpoint: set the base URL, key, wires, and
                  per-model prices yourself.
                </span>
              </button>
              {filtered.map(({ vendor, service }) => (
                <PresetCard
                  key={`${vendor.id}/${service.id}`}
                  vendor={vendor}
                  service={service}
                  added={addedCatalogIds.has(service.id)}
                  onAdd={() => openAdd(service)}
                />
              ))}
            </div>
          )}
        </>
      )}

    </Page>
  );
}

interface PresetCardProps {
  vendor: CatalogVendor;
  service: CatalogService;
  added: boolean;
  onAdd: () => void;
}

function PresetCard({ vendor, service, added, onAdd }: PresetCardProps) {
  const shown = service.models.slice(0, 4);
  return (
    <div className={`card ${styles.entry}`}>
      <div className={styles.entryHead}>
        <div className={styles.entryTitle}>
          <span className={styles.vendorName}>{vendor.name}</span>
          <span className={styles.serviceName}>{service.name}</span>
        </div>
        {added && (
          <span className={styles.addedChip}>
            <Check size={12} /> Added
          </span>
        )}
      </div>

      <div className={styles.tags}>
        <span className="chip">{service.kind}</span>
        <span className="chip">{service.adapter}</span>
      </div>

      <div className={styles.baseUrl}>{service.base_url}</div>
      {service.note && <div className={styles.note}>{service.note}</div>}

      <div className={styles.models}>
        {shown.map((m) => (
          <span key={m.model} className="chip chip-mono">
            {m.model}
          </span>
        ))}
        {service.models.length > shown.length && (
          <span className="chip">+{service.models.length - shown.length}</span>
        )}
      </div>

      <div className={styles.entryFoot}>
        {service.docs ? (
          <a className={styles.docs} href={service.docs} target="_blank" rel="noreferrer">
            Docs
          </a>
        ) : (
          <span />
        )}
        <button className="btn btn-sm btn-primary" onClick={onAdd}>
          <Plus size={13} /> Add provider
        </button>
      </div>
    </div>
  );
}

interface ProviderCardProps {
  provider: Provider;
  onChanged: () => void;
  onEdit: () => void;
  onDelete: () => Promise<void>;
}

function ProviderCard({ provider, onChanged, onEdit, onDelete }: ProviderCardProps) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<VendorTestResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const toast = useToast();

  const confirmDelete = async () => {
    setDeleting(true);
    try {
      await onDelete();
    } finally {
      setDeleting(false);
      setConfirmingDelete(false);
    }
  };

  const { stats } = provider;
  const complete = provider.masked_key !== '' && provider.models.length > 0;

  const runTest = async () => {
    setTesting(true);
    setResult(null);
    try {
      setResult(await api.testProvider(provider.id));
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

  const toggleEnabled = async () => {
    setBusy(true);
    try {
      await api.patchProvider(provider.id, { enabled: !provider.enabled });
      onChanged();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Update failed.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={`card ${styles.provider} ${provider.enabled ? '' : styles.disabled}`}>
      <div className={styles.head}>
        <div className={styles.headLeft}>
          <div className={styles.nameRow}>
            <span className={styles.name}>{provider.name}</span>
            <span className="chip">{provider.adapter}</span>
            {provider.vendor && <span className={styles.vendorTag}>{provider.vendor}</span>}
            {!provider.enabled ? (
              <span className={`${styles.badge} ${styles.off}`}>Disabled</span>
            ) : !complete ? (
              <span className={`${styles.badge} ${styles.draft}`}>Draft</span>
            ) : (
              <span className={`${styles.badge} ${stats.healthy ? styles.healthy : styles.unhealthy}`}>
                {stats.healthy ? <CheckCircle2 size={12} /> : <XCircle size={12} />}
                {stats.healthy ? 'Healthy' : 'Unhealthy'}
              </span>
            )}
          </div>
          <div className={styles.providerBaseUrl}>{provider.base_url}</div>
        </div>
        <div className={styles.headRight}>
          {confirmingDelete ? (
            <>
              <button
                className="btn btn-sm btn-danger"
                disabled={deleting}
                title="Its key and prices are deleted and it stops routing immediately. This cannot be undone."
                onClick={confirmDelete}
              >
                {deleting ? 'Removing…' : 'Confirm remove'}
              </button>
              <button
                className="btn btn-sm"
                disabled={deleting}
                onClick={() => setConfirmingDelete(false)}
              >
                Cancel
              </button>
            </>
          ) : (
            <>
              <button className="btn btn-sm" onClick={runTest} disabled={testing}>
                {testing ? <span className="spinner" style={{ width: 13, height: 13 }} /> : null}
                {testing ? 'Testing…' : 'Test'}
              </button>
              <button className="btn btn-sm" onClick={toggleEnabled} disabled={busy}>
                {provider.enabled ? 'Disable' : 'Enable'}
              </button>
              <button className="btn btn-sm" onClick={onEdit}>
                <Pencil size={12} /> Edit
              </button>
              <button
                className="btn btn-sm btn-danger"
                aria-label="Remove provider"
                onClick={() => setConfirmingDelete(true)}
              >
                <Trash2 size={12} />
              </button>
            </>
          )}
        </div>
      </div>

      {result && (
        <div
          className={`${styles.testResult} ${result.reachable ? styles.testOk : styles.testErr}`}
        >
          {result.reachable
            ? `reachable · ${result.status} · ${ms(result.latency_ms)}`
            : result.error || `unreachable · ${result.status}`}
        </div>
      )}

      <div className={styles.body}>
        <div className={styles.section}>
          <div className={styles.metaRow}>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Priority</span>
              <span className={styles.metaValue}>{provider.priority}</span>
            </div>
            <div className={styles.metaItem}>
              <span className={styles.metaLabel}>Weight</span>
              <span className={styles.metaValue}>{provider.weight}</span>
            </div>
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

          <span className={styles.sectionTitle}>Wires</span>
          <div className={styles.creds} style={{ marginBottom: 10 }}>
            {provider.wires.length === 0 ? (
              <span className="muted" style={{ fontSize: 12 }}>
                No wires enabled — all paths are denied.
              </span>
            ) : (
              provider.wires.map((w) => (
                <span key={w} className="chip chip-mono" style={{ fontSize: 11 }}>
                  {w}
                </span>
              ))
            )}
            {provider.allow_unmatched && (
              <span
                className="chip"
                style={{ fontSize: 11, color: 'var(--danger, #b54)', fontWeight: 600 }}
                title="Unmatched paths are forwarded but metered zero"
              >
                allow unmatched
              </span>
            )}
          </div>

          <span className={styles.sectionTitle}>API key</span>
          <div className={styles.creds}>
            {provider.masked_key === '' ? (
              <span className="muted" style={{ fontSize: 12 }}>
                No key — edit the provider and paste one to start routing.
              </span>
            ) : (
              <span className={styles.cred}>
                <KeyRound size={12} />
                {provider.masked_key}
              </span>
            )}
          </div>
        </div>

        <div className={styles.section}>
          <span className={styles.sectionTitle}>Models &amp; prices</span>
          {provider.models.length === 0 ? (
            <span className="muted" style={{ fontSize: 12 }}>
              No models configured.
            </span>
          ) : (
            <table className={styles.priceTable}>
              <thead>
                <tr>
                  <th>Model</th>
                  <th style={{ textAlign: 'right' }}>Input</th>
                  <th style={{ textAlign: 'right' }}>Output</th>
                  <th style={{ textAlign: 'right' }}>Cached</th>
                  <th>Unit</th>
                </tr>
              </thead>
              <tbody>
                {provider.models.map((m) => (
                  <tr key={m.model}>
                    <td className="mono">{m.model}</td>
                    <td className="n">{m.input}</td>
                    <td className="n">{m.output}</td>
                    <td className="n">{m.cached_input || '—'}</td>
                    <td className="mono">{m.unit}</td>
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
