import { useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Check, Layers, Minus, Plus, Trash2 } from 'lucide-react';
import { api } from '../api/client';
import type {
  Catalog,
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
import { wireName, wireServesModels, wireIsUtilityToggle, wireUtilityNote } from '../lib/wires';
import styles from './VendorAdd.module.css';

export function VendorAddPage() {
  const { vendorId } = useParams<{ vendorId: string }>();
  const navigate = useNavigate();
  const toast = useToast();
  const catalog = useFetch(() => api.catalog(), []);
  const providers = useFetch(() => api.providers(), []);

  const vendor = catalog.data?.vendors.find((v) => v.id === vendorId);
  const custom = !!vendor?.custom;
  const sameVendorCount = (providers.data ?? []).filter((p) => p.catalog_id === vendorId).length;
  // Catalog vendors auto-take the vendor name for their first provider; a custom
  // template has no meaningful default, so it always asks. Once a catalog vendor
  // already has a provider the name would collide, so we ask for a distinct one.
  const needsName = custom || sameVendorCount > 0;
  const suggestedName = custom ? 'my-provider' : vendor ? `${vendor.name} ${sameVendorCount + 1}` : '';

  // Prices for user-typed custom models are borrowed from the built-in catalog by
  // model id, so the user never re-enters known prices.
  const priceIndex = useMemo(() => buildPriceIndex(catalog.data), [catalog.data]);

  // Model-bearing wires (the cards). Catalog vendors only show wires that already
  // list models; a custom template shows every model-serving wire so the user can
  // pick which protocol(s) their endpoint speaks. Companion wires (model listings)
  // ride along on submit when they share a host + adapter with a selected wire.
  const modelWires = useMemo(
    () =>
      vendor
        ? vendor.endpoints.filter(
            (ep) =>
              wireServesModels(ep.wire) && (custom || (ep.models ?? []).length > 0),
          )
        : [],
    [vendor, custom],
  );

  // Utility wires (model-less on/off toggles, e.g. Anthropic count_tokens). Shown
  // as their own cards. Default on for catalog vendors (the endpoint is known
  // good); off for custom templates, where the user opts in per their base URL.
  const utilityWires = useMemo(
    () => (vendor ? vendor.endpoints.filter((ep) => wireIsUtilityToggle(ep.wire)) : []),
    [vendor],
  );

  // --- Catalog-vendor selection (per wire+model checkbox) ---
  const allKeys = useMemo(
    () =>
      new Set([
        ...modelWires.flatMap((ep) => (ep.models ?? []).map((m) => mkey(ep.wire, m))),
        ...(custom ? [] : utilityWires.map((ep) => mkey(ep.wire, ''))),
      ]),
    [modelWires, utilityWires, custom],
  );
  const [selected, setSelected] = useState<Set<string> | null>(null);
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
  const toggleUtility = (wire: string) =>
    setSelected(() => {
      const next = new Set(checked);
      const k = mkey(wire, '');
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  const enabledUtility = () => utilityWires.map((ep) => ep.wire).filter((wire) => checked.has(mkey(wire, '')));

  // --- Custom-template state (base URL + free-form models per wire) ---
  const [baseUrl, setBaseUrl] = useState('');
  const [customModels, setCustomModels] = useState<Record<string, string[]>>({});
  const rowsFor = (wire: string) => customModels[wire] ?? [];
  const addModelRow = (wire: string) =>
    setCustomModels((p) => ({ ...p, [wire]: [...(p[wire] ?? []), ''] }));
  const setModelRow = (wire: string, i: number, value: string) =>
    setCustomModels((p) => ({ ...p, [wire]: (p[wire] ?? []).map((m, idx) => (idx === i ? value : m)) }));
  const removeModelRow = (wire: string, i: number) =>
    setCustomModels((p) => ({ ...p, [wire]: (p[wire] ?? []).filter((_, idx) => idx !== i) }));

  const [name, setName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

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

    const providerName = needsName ? name.trim() : vendor.name;
    if (needsName && !providerName) {
      setErr('Enter a name for this provider.');
      return;
    }

    let body: CreateProviderBody;
    if (custom) {
      const url = baseUrl.trim();
      try {
        const u = new URL(url);
        if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error('scheme');
      } catch {
        setErr('Base URL must be an absolute http(s) URL.');
        return;
      }
      const wireModels = customWireModels(modelWires, customModels);
      const { endpoints, models } = buildProvider(vendor, wireModels, (id) => customPrice(id, priceIndex), url, enabledUtility());
      if (models.length === 0) {
        setErr('Add at least one model.');
        return;
      }
      body = { name: providerName, enabled: true, api_key: apiKey.trim() || undefined, models, endpoints };
    } else {
      const wireModels = catalogWireModels(vendor, checked);
      const { endpoints, models } = buildProvider(vendor, wireModels, (id) => catalogPrice(id, vendor), null, enabledUtility());
      if (models.length === 0) {
        setErr('Select at least one model.');
        return;
      }
      body = {
        name: providerName,
        vendor: vendor.name,
        catalog_id: vendor.id,
        quirks: vendor.quirks,
        api_key: apiKey.trim() || undefined,
        models,
        endpoints,
      };
    }

    setBusy(true);
    setErr(null);
    try {
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
      title={custom ? 'Add a custom provider' : `Add ${vendor.name}`}
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
              {custom
                ? 'Unique display name, shown in stats and the call ledger.'
                : `This vendor already has ${sameVendorCount === 1 ? 'a provider' : `${sameVendorCount} providers`}; pick a distinct name.`}
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

        {custom && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="v-base">Base URL</label>
            <input
              id="v-base"
              className={`input mono ${styles.keyInput}`}
              type="text"
              value={baseUrl}
              placeholder="https://your-endpoint.example"
              onChange={(e) => setBaseUrl(e.target.value)}
            />
            <span className={styles.hint}>
              The bare host; each wire's full path is appended (e.g.{' '}
              <code>{resolveEndpoint('{base}/v1/chat/completions', baseUrl.trim() || '…')}</code>).
            </span>
          </div>
        )}

        <div className={styles.field}>
          <span className={styles.label}>APIs</span>
          {custom && (
            <span className={styles.hint}>
              Add models under the API(s) your endpoint serves; empty APIs are skipped.
            </span>
          )}
          <div className={styles.apiGrid}>
            {modelWires.map((ep) =>
              custom
                ? renderCustomCard(ep)
                : renderCatalogCard(ep),
            )}
            {utilityWires.map((ep) => renderUtilityCard(ep))}
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

  // Catalog card: checkboxes over the preset's models.
  function renderCatalogCard(ep: CatalogEndpoint) {
    const models = ep.models ?? [];
    const keys = models.map((m) => mkey(ep.wire, m));
    const onCount = keys.filter((k) => checked.has(k)).length;
    const allOn = onCount === keys.length;
    const someOn = onCount > 0;
    return (
      <div key={ep.wire} className={`card ${styles.apiCard} ${someOn ? styles.apiCardOn : ''}`}>
        <button type="button" className={styles.apiCardHead} aria-pressed={allOn} onClick={() => toggleWire(ep)}>
          <span className={styles.apiName}>{wireName(ep.wire, vendor!.id)}</span>
          <span className={`${styles.apiCheck} ${someOn ? styles.apiCheckOn : ''}`}>
            {allOn ? <Check size={13} strokeWidth={3} /> : someOn ? <Minus size={13} strokeWidth={3} /> : null}
          </span>
        </button>
        <span className={styles.apiUrl}>{ep.endpoint}</span>
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
  }

  // Custom card: free-form model rows the user types, with borrowed prices.
  function renderCustomCard(ep: CatalogEndpoint) {
    const rows = rowsFor(ep.wire);
    const filled = rows.some((m) => m.trim());
    const resolved = resolveEndpoint(ep.endpoint, baseUrl.trim() || '…');
    return (
      <div key={ep.wire} className={`card ${styles.apiCard} ${filled ? styles.apiCardOn : ''}`}>
        <div className={styles.apiCardHead}>
          <span className={styles.apiName}>{wireName(ep.wire, vendor!.id)}</span>
        </div>
        <span className={styles.apiUrl}>{resolved}</span>
        <div className={styles.modelList}>
          {rows.map((m, i) => {
            const price = priceIndex[m.trim()];
            return (
              <div key={i} className={styles.modelInputRow}>
                <input
                  className={`input mono ${styles.modelInput}`}
                  value={m}
                  placeholder="model-id"
                  onChange={(e) => setModelRow(ep.wire, i, e.target.value)}
                />
                <span className={styles.modelPrice}>
                  {m.trim() ? (price ? `in ${price.input} · out ${price.output}` : 'unpriced') : ''}
                </span>
                <button
                  type="button"
                  className={styles.iconBtn}
                  aria-label="Remove model"
                  onClick={() => removeModelRow(ep.wire, i)}
                >
                  <Trash2 size={13} />
                </button>
              </div>
            );
          })}
          <button type="button" className={styles.addBtn} onClick={() => addModelRow(ep.wire)}>
            <Plus size={13} /> Add model
          </button>
        </div>
      </div>
    );
  }

  // Utility card: a model-less wire (e.g. count_tokens) toggled on/off as a whole.
  function renderUtilityCard(ep: CatalogEndpoint) {
    const on = checked.has(mkey(ep.wire, ''));
    const url = resolveEndpoint(ep.endpoint, custom ? baseUrl.trim() || '…' : null);
    return (
      <div key={ep.wire} className={`card ${styles.apiCard} ${on ? styles.apiCardOn : ''}`}>
        <button
          type="button"
          className={styles.apiCardHead}
          aria-pressed={on}
          onClick={() => toggleUtility(ep.wire)}
        >
          <span className={styles.apiName}>{wireName(ep.wire, vendor!.id)}</span>
          <span className={`${styles.apiCheck} ${on ? styles.apiCheckOn : ''}`}>
            {on ? <Check size={13} strokeWidth={3} /> : null}
          </span>
        </button>
        <span className={styles.apiUrl}>{url}</span>
        <div className={styles.utilityNote}>{wireUtilityNote(ep.wire)}</div>
      </div>
    );
  }
}

// Selection key for one (wire, model) checkbox. A space can't appear in a wire id
// or model name, so it's a safe separator. Shared with the edit form so it
// pre-checks the same keys.
export const mkey = (wire: string, model: string) => `${wire} ${model}`;

/** Per-model price borrowed from the catalog for custom providers. */
export interface CatalogPrice {
  input: number;
  output: number;
  cached_input: number;
  unit: string;
  price_override?: boolean;
}

// Flatten every catalog vendor's price list into one model-id → price index.
// The first vendor to define a model id wins.
export function buildPriceIndex(catalog: Catalog | null | undefined): Record<string, CatalogPrice> {
  const index: Record<string, CatalogPrice> = {};
  for (const vendor of catalog?.vendors ?? []) {
    for (const [id, m] of Object.entries(vendor.models)) {
      if (index[id]) continue;
      index[id] = { input: m.input, output: m.output, cached_input: m.cached_input ?? 0, unit: m.unit };
    }
  }
  return index;
}

// catalogWireModels: the (wire → checked model ids) map for a catalog vendor.
function catalogWireModels(vendor: CatalogVendor, checked: Set<string>): Map<string, string[]> {
  const map = new Map<string, string[]>();
  for (const ep of vendor.endpoints) {
    if (!wireServesModels(ep.wire)) continue;
    const picked = (ep.models ?? []).filter((m) => checked.has(mkey(ep.wire, m)));
    if (picked.length) map.set(ep.wire, picked);
  }
  return map;
}

// customWireModels: the (wire → typed model ids) map for a custom template,
// trimmed and de-duplicated per wire.
function customWireModels(
  modelWires: CatalogEndpoint[],
  customModels: Record<string, string[]>,
): Map<string, string[]> {
  const map = new Map<string, string[]>();
  for (const ep of modelWires) {
    const seen = new Set<string>();
    const ids: string[] = [];
    for (const raw of customModels[ep.wire] ?? []) {
      const m = raw.trim();
      if (!m || seen.has(m)) continue;
      seen.add(m);
      ids.push(m);
    }
    if (ids.length) map.set(ep.wire, ids);
  }
  return map;
}

function catalogPrice(id: string, vendor: CatalogVendor): ProviderModel | null {
  const m = vendor.models[id];
  return m
    ? { model: id, input: m.input, output: m.output, cached_input: m.cached_input ?? 0, unit: m.unit }
    : null;
}

function customPrice(id: string, priceIndex: Record<string, CatalogPrice>): ProviderModel {
  const p = priceIndex[id];
  return {
    model: id,
    input: p?.input ?? 0,
    output: p?.output ?? 0,
    cached_input: p?.cached_input ?? 0,
    unit: p?.unit ?? 'per_1m_tokens',
  };
}

// buildProvider turns a (wire → model ids) selection into the endpoints + models
// to POST: each selected model-serving wire is included, pulls in any silent
// companion (non-model) wire sharing its (origin, adapter) group — the same
// grouping the backend uses to form routing vendors — and only the selected
// models are priced. Utility wires (model-less toggles like count_tokens) are the
// exception: they never ride along silently and are included only when explicitly
// enabled via `utilityWires`. `base` (non-null for custom templates) substitutes
// the {base} URL placeholder.
export function buildProvider(
  vendor: CatalogVendor,
  wireModels: Map<string, string[]>,
  priceModel: (id: string) => ProviderModel | null,
  base: string | null,
  utilityWires: Iterable<string> = [],
): { endpoints: ProviderEndpoint[]; models: ProviderModel[] } {
  const selectedWires = new Set([...wireModels.keys()]);
  const enabledUtility = new Set(utilityWires);
  const selectedGroups = new Set(
    vendor.endpoints.filter((ep) => selectedWires.has(ep.wire)).map((ep) => groupKey(ep, base)),
  );
  const endpoints: ProviderEndpoint[] = [];
  for (const ep of vendor.endpoints) {
    const include = selectedWires.has(ep.wire)
      ? true
      : wireIsUtilityToggle(ep.wire)
        ? enabledUtility.has(ep.wire)
        : !wireServesModels(ep.wire) && selectedGroups.has(groupKey(ep, base));
    if (!include) continue;
    endpoints.push({ wire: ep.wire, endpoint: resolveEndpoint(ep.endpoint, base), adapter: ep.adapter });
  }
  const ids = new Set<string>();
  for (const list of wireModels.values()) for (const id of list) ids.add(id);
  const models: ProviderModel[] = [];
  for (const id of ids) {
    const m = priceModel(id);
    if (m) models.push(m);
  }
  return { endpoints, models };
}

// resolveEndpoint substitutes the {base} placeholder with the user's base URL
// (trailing slashes trimmed). Catalog vendors pass base=null and are unchanged.
export function resolveEndpoint(endpoint: string, base: string | null): string {
  return base === null ? endpoint : endpoint.replace('{base}', base.replace(/\/+$/, ''));
}

// groupKey is an endpoint's (origin, adapter) — the key the backend groups by to
// form routing vendors. Used to decide which companion wires ride along.
function groupKey(ep: CatalogEndpoint, base: string | null): string {
  return `${originOf(resolveEndpoint(ep.endpoint, base))}\n${ep.adapter}`;
}

// originOf is the scheme://host of an endpoint URL (a {model} placeholder is
// stubbed first so the URL parses). Falls back to the raw string if unparseable.
function originOf(endpoint: string): string {
  try {
    return new URL(endpoint.replace('{model}', 'MODEL')).origin;
  } catch {
    return endpoint;
  }
}
