import { useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Check, Layers, Minus } from 'lucide-react';
import { api } from '../api/client';
import type {
  CatalogEndpoint,
  CatalogVendor,
  CreateProviderBody,
  ProviderEndpoint,
  ProviderModel,
} from '../api/types';
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
  const sameVendorCount = (providers.data ?? []).filter((p) => p.catalog_id === vendorId).length;
  // The first provider of a vendor takes the vendor name automatically; once one
  // exists, the name would collide, so we ask for a distinct one.
  const needsName = sameVendorCount > 0;
  const suggestedName = vendor ? `${vendor.name} ${sameVendorCount + 1}` : '';

  // Model-bearing wires (the cards). Companion wires (model listings) ride
  // along on submit when they share a base URL with a selected wire.
  const modelWires = useMemo(
    () =>
      vendor
        ? vendor.endpoints.filter((ep) => wireServesModels(ep.wire) && (ep.models ?? []).length > 0)
        : [],
    [vendor],
  );

  // Selection is per (wire, model) pair, since the same model can be served by
  // more than one wire and we want each card's checkboxes independent.
  const allKeys = useMemo(
    () => new Set(modelWires.flatMap((ep) => (ep.models ?? []).map((m) => mkey(ep.wire, m)))),
    [modelWires],
  );

  const [selected, setSelected] = useState<Set<string> | null>(null);
  const [name, setName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Default every model on, once the catalog has loaded.
  const checked = selected ?? allKeys;
  const toggleModel = (wire: string, model: string) =>
    setSelected(() => {
      const next = new Set(checked);
      const k = mkey(wire, model);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  const toggleWire = (ep: CatalogEndpoint) =>
    setSelected(() => {
      const next = new Set(checked);
      const keys = (ep.models ?? []).map((m) => mkey(ep.wire, m));
      const allOn = keys.every((k) => next.has(k));
      for (const k of keys) {
        if (allOn) next.delete(k);
        else next.add(k);
      }
      return next;
    });

  if (catalog.error) return <Page title="Add provider"><ErrorBanner message={catalog.error} onRetry={catalog.refetch} /></Page>;
  if (catalog.initialLoading) {
    return (
      <Page title="Add provider">
        <div className={`card ${styles.card}`} style={{ padding: 20 }}>
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
          ))}
        </div>
      </Page>
    );
  }
  if (!vendor) {
    return (
      <Page title="Add provider" actions={<Link to="/providers" className="btn"><ArrowLeft size={15} /> Back</Link>}>
        <EmptyState icon={Layers} title="Vendor not found" hint="It may have been removed from the catalog." />
      </Page>
    );
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const { endpoints, models } = buildProvider(vendor, checked);
    if (models.length === 0) {
      setErr('Select at least one model.');
      return;
    }
    const providerName = needsName ? name.trim() : vendor.name;
    if (needsName && !providerName) {
      setErr('Enter a name for this provider.');
      return;
    }

    setBusy(true);
    setErr(null);
    try {
      const body: CreateProviderBody = {
        name: providerName,
        vendor: vendor.name,
        catalog_id: vendor.id,
        quirks: vendor.quirks,
        api_key: apiKey.trim() || undefined,
        models,
        endpoints,
      };
      await api.createProvider(body);
      toast.success(`Added ${providerName}.`);
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
        <Link to="/providers" className="btn">
          <ArrowLeft size={15} /> Back to providers
        </Link>
      }
    >
      <form className={`card ${styles.card}`} onSubmit={submit}>
        <div className={styles.vendorHead}>
          <BrandIcon brand={providerBrand(vendor.name, Object.keys(vendor.models))} label={vendor.name} size={24} />
          <span className={styles.vendorName}>{vendor.name}</span>
        </div>

        {needsName && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="v-name">Name</label>
            <input
              id="v-name"
              className={`input ${styles.keyInput}`}
              type="text"
              value={name}
              placeholder={suggestedName}
              onChange={(e) => setName(e.target.value)}
            />
            <span className={styles.hint}>
              This vendor already has {sameVendorCount === 1 ? 'a provider' : `${sameVendorCount} providers`}; pick a distinct name.
            </span>
          </div>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="v-key">API key</label>
          <input
            id="v-key"
            className={`input mono ${styles.keyInput}`}
            type="password"
            value={apiKey}
            placeholder="sk-…"
            onChange={(e) => setApiKey(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <span className={styles.label}>APIs</span>
          <div className={styles.apiGrid}>
            {modelWires.map((ep) => {
              const models = ep.models ?? [];
              const keys = models.map((m) => mkey(ep.wire, m));
              const onCount = keys.filter((k) => checked.has(k)).length;
              const allOn = onCount === keys.length;
              const someOn = onCount > 0;
              return (
                <div
                  key={ep.wire}
                  className={`card ${styles.apiCard} ${someOn ? styles.apiCardOn : ''}`}
                >
                  <button
                    type="button"
                    className={styles.apiCardHead}
                    aria-pressed={allOn}
                    onClick={() => toggleWire(ep)}
                  >
                    <span className={styles.apiName}>{wireName(ep.wire)}</span>
                    <span className={`${styles.apiCheck} ${someOn ? styles.apiCheckOn : ''}`}>
                      {allOn ? <Check size={13} strokeWidth={3} /> : someOn ? <Minus size={13} strokeWidth={3} /> : null}
                    </span>
                  </button>
                  <span className={styles.apiUrl}>{ep.base_url}</span>
                  <div className={styles.modelList}>
                    {models.map((m) => {
                      const on = checked.has(mkey(ep.wire, m));
                      return (
                        <label key={m} className={styles.modelRow}>
                          <span className={styles.modelName}>{m}</span>
                          <input
                            type="checkbox"
                            className={styles.srOnly}
                            checked={on}
                            onChange={() => toggleModel(ep.wire, m)}
                          />
                          <span className={`${styles.modelCheck} ${on ? styles.apiCheckOn : ''}`}>
                            {on && <Check size={11} strokeWidth={3} />}
                          </span>
                        </label>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        {err && <div className={styles.error}>{err}</div>}

        <div className={styles.footerRow}>
          <button type="button" className="btn" onClick={() => navigate('/providers')} disabled={busy}>
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

// Selection key for one (wire, model) checkbox. NUL can't appear in a wire id
// or model name, so it's a safe separator.
const mkey = (wire: string, model: string) => `${wire} ${model}`;

// buildProvider turns the per-(wire, model) selection into the endpoints +
// models to POST: a wire is included when ≥1 of its models is checked, each
// included wire pulls in any companion (non-model) wire sharing its base URL,
// and only the checked models are priced.
function buildProvider(vendor: CatalogVendor, checked: Set<string>): {
  endpoints: ProviderEndpoint[];
  models: ProviderModel[];
} {
  const selectedWires = new Set<string>();
  const modelIds = new Set<string>();
  for (const ep of vendor.endpoints) {
    if (!wireServesModels(ep.wire)) continue;
    const picked = (ep.models ?? []).filter((m) => checked.has(mkey(ep.wire, m)));
    if (picked.length === 0) continue;
    selectedWires.add(ep.wire);
    for (const m of picked) modelIds.add(m);
  }
  const selectedBases = new Set(
    vendor.endpoints.filter((ep) => selectedWires.has(ep.wire)).map((ep) => ep.base_url),
  );
  const endpoints: ProviderEndpoint[] = [];
  for (const ep of vendor.endpoints) {
    const include = selectedWires.has(ep.wire) || (!wireServesModels(ep.wire) && selectedBases.has(ep.base_url));
    if (!include) continue;
    endpoints.push({ wire: ep.wire, base_url: ep.base_url, adapter: ep.adapter });
  }
  const models: ProviderModel[] = [];
  for (const id of modelIds) {
    const m = vendor.models[id];
    if (!m) continue;
    models.push({ model: id, input: m.input, output: m.output, cached_input: m.cached_input ?? 0, unit: m.unit });
  }
  return { endpoints, models };
}
