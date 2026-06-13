import { useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Layers } from 'lucide-react';
import { api } from '../api/client';
import type { CatalogVendor, CreateProviderBody, ProviderEndpoint, ProviderModel } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { BrandIcon, providerBrand } from '../lib/modelBrand';
import { wireName, wireServesModels } from '../lib/wires';
import styles from './VendorAdd.module.css';

export function VendorAddPage() {
  const { vendorId } = useParams<{ vendorId: string }>();
  const navigate = useNavigate();
  const toast = useToast();
  const catalog = useFetch(() => api.catalog(), []);
  const providers = useFetch(() => api.providers(), []);

  const vendor = catalog.data?.vendors.find((v) => v.id === vendorId);
  const alreadyAdded = (providers.data ?? []).some((p) => p.catalog_id === vendorId);

  // Capability wires (the checkboxes); companion wires ride along on submit.
  const capWires = useMemo(
    () => (vendor ? vendor.endpoints.filter((ep) => wireServesModels(ep.wire)).map((ep) => ep.wire) : []),
    [vendor],
  );

  const [selected, setSelected] = useState<Set<string> | null>(null);
  const [apiKey, setApiKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Default every capability wire on, once the catalog has loaded.
  const checked = selected ?? new Set(capWires);
  const toggle = (w: string) =>
    setSelected(() => {
      const next = new Set(checked);
      if (next.has(w)) next.delete(w);
      else next.add(w);
      return next;
    });

  if (catalog.error) return <Page title="Add provider"><ErrorBanner message={catalog.error} onRetry={catalog.refetch} /></Page>;
  if (catalog.initialLoading) {
    return (
      <Page title="Add provider">
        <div className="card" style={{ maxWidth: 720, padding: 20 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      </Page>
    );
  }
  if (!vendor) {
    return (
      <Page title="Add provider" actions={<Link to="/providers/add" className="btn"><ArrowLeft size={15} /> Back</Link>}>
        <EmptyState icon={Layers} title="Vendor not found" hint="It may have been removed from the catalog." />
      </Page>
    );
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const wires = capWires.filter((w) => checked.has(w));
    if (wires.length === 0) {
      setErr('Select at least one wire.');
      return;
    }
    const { endpoints, models } = buildProvider(vendor, wires);

    setBusy(true);
    setErr(null);
    try {
      const body: CreateProviderBody = {
        name: vendor.id,
        vendor: vendor.name,
        catalog_id: vendor.id,
        quirks: vendor.quirks,
        api_key: apiKey.trim() || undefined,
        models,
        endpoints,
      };
      await api.createProvider(body);
      toast.success(`Added ${vendor.name}.`);
      navigate('/providers');
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <Page
      title={`Add ${vendor.name}`}
      actions={
        <Link to="/providers/add" className="btn">
          <ArrowLeft size={15} /> Back to vendors
        </Link>
      }
    >
      <form className={`card ${styles.card}`} onSubmit={submit}>
        <div className={styles.vendorHead}>
          <BrandIcon brand={providerBrand(vendor.name, Object.keys(vendor.models))} label={vendor.name} size={24} />
          <span className={styles.vendorName}>{vendor.name}</span>
        </div>

        {alreadyAdded && (
          <div className={styles.notice}>
            This vendor already has a provider. Adding again will fail on the duplicate name —{' '}
            <Link to="/providers">edit the existing one</Link> instead.
          </div>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="v-key">API key</label>
          <input
            id="v-key"
            className="input mono"
            type="password"
            value={apiKey}
            placeholder="paste your key — one key for this vendor"
            onChange={(e) => setApiKey(e.target.value)}
          />
          <span className={styles.hint}>Stored as-is; shown masked afterwards. Shared by all selected wires.</span>
        </div>

        <div className={styles.field}>
          <span className={styles.label}>Wires</span>
          <span className={styles.hint}>
            Pick the API surfaces you want. Base URLs and prices are preset; model-listing
            endpoints are included automatically.
          </span>
          <div className={styles.wireList}>
            {vendor.endpoints
              .filter((ep) => wireServesModels(ep.wire))
              .map((ep) => (
                <label key={ep.wire} className={styles.wireItem}>
                  <input type="checkbox" checked={checked.has(ep.wire)} onChange={() => toggle(ep.wire)} />
                  <span className={styles.wireMain}>
                    <span className={styles.wireLabel}>{wireName(ep.wire)}</span>
                    <span className={styles.wireUrl}>{ep.base_url}</span>
                  </span>
                  <span className={styles.wireModels}>
                    {(ep.models ?? []).length} {(ep.models ?? []).length === 1 ? 'model' : 'models'}
                  </span>
                </label>
              ))}
          </div>
        </div>

        {err && <div className={styles.error}>{err}</div>}

        <div className={styles.footerRow}>
          <button type="button" className="btn" onClick={() => navigate('/providers/add')} disabled={busy}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {busy ? 'Adding…' : 'Add provider'}
          </button>
        </div>
      </form>
    </Page>
  );
}

// buildProvider turns the selected capability wires into the endpoints + models
// to POST: it adds each selected wire's endpoint plus any companion (non-model)
// wires sharing its base URL, and prices the union of referenced models.
function buildProvider(vendor: CatalogVendor, selectedWires: string[]): {
  endpoints: ProviderEndpoint[];
  models: ProviderModel[];
} {
  const selectedBases = new Set(
    vendor.endpoints.filter((ep) => selectedWires.includes(ep.wire)).map((ep) => ep.base_url),
  );
  const endpoints: ProviderEndpoint[] = [];
  const modelIds = new Set<string>();
  for (const ep of vendor.endpoints) {
    const include = selectedWires.includes(ep.wire) || (!wireServesModels(ep.wire) && selectedBases.has(ep.base_url));
    if (!include) continue;
    endpoints.push({ wire: ep.wire, base_url: ep.base_url, adapter: ep.adapter });
    for (const m of ep.models ?? []) modelIds.add(m);
  }
  const models: ProviderModel[] = [];
  for (const id of modelIds) {
    const m = vendor.models[id];
    if (!m) continue;
    models.push({ model: id, input: m.input, output: m.output, cached_input: m.cached_input ?? 0, unit: m.unit });
  }
  return { endpoints, models };
}
