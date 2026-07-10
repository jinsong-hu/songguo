import { useMemo, useState, type FormEvent } from 'react';
import { Check, Minus, Plus, Trash2 } from 'lucide-react';
import { api } from '../api/client';
import type {
  CatalogEndpoint,
  CatalogVendor,
  PatchProviderBody,
  Provider,
  ProviderEndpoint,
} from '../api/types';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import { useFetch } from '../lib/useFetch';
import { BrandIcon, providerBrand } from '../lib/modelBrand';
import { wireName, wireServesModels, wireIsUtilityToggle, wireUtilityNote } from '../lib/wires';
import {
  buildPriceIndex,
  buildProvider,
  mkey,
  resolveEndpoint,
  type CatalogPrice,
} from '../pages/VendorAdd';
import cards from '../pages/VendorAdd.module.css';
import styles from './ProviderForm.module.css';

const UNITS = [
  'per_1m_tokens',
  'per_1k_tokens',
  'per_token',
  'per_call',
  'per_image',
  'per_second',
  'per_char',
];

interface ProviderFormProps {
  editing: Provider;
  onCancel: () => void;
  onSaved: (provider: Provider) => void;
  onDeleted?: () => void;
}

/**
 * Card-based provider editor — same layout as the add page (VendorAdd): a vendor
 * head, key, and a grid of API cards, with the routing/price knobs the add flow
 * hides listed flat below. Catalog-backed providers re-use their catalog vendor's
 * card menu (checkboxes pre-checked to the served models); custom providers use
 * the `custom` template (free-form model rows) with the base URL recovered from
 * their saved endpoints.
 */
export function ProviderForm({ editing, onCancel, onSaved, onDeleted }: ProviderFormProps) {
  const catalog = useFetch(() => api.catalog(), []);

  const priceIndex = useMemo(() => buildPriceIndex(catalog.data), [catalog.data]);

  // The vendor whose card menu we render. A matching, non-custom catalog vendor →
  // catalog mode (checkboxes over its presets). Otherwise → custom mode, driven by
  // the `custom` template's wire list.
  const catalogVendor = useMemo(
    () => catalog.data?.vendors.find((v) => v.id === editing.catalog_id && !v.custom),
    [catalog.data, editing.catalog_id],
  );
  const customVendor = useMemo(
    () => catalog.data?.vendors.find((v) => v.custom),
    [catalog.data],
  );
  const isCustom = !catalogVendor;
  const vendor = catalogVendor ?? customVendor ?? null;

  // Model-serving wires shown as cards. Catalog: presets that list models. Custom:
  // every model-serving wire in the template (so the user can add new protocols).
  const modelWires = useMemo(
    () =>
      vendor
        ? vendor.endpoints.filter(
            (ep) => wireServesModels(ep.wire) && (isCustom || (ep.models ?? []).length > 0),
          )
        : [],
    [vendor, isCustom],
  );

  // Utility wires (model-less on/off toggles, e.g. count_tokens), shown as cards
  // in both catalog and custom modes.
  const utilityWires = useMemo(
    () => (vendor ? vendor.endpoints.filter((ep) => wireIsUtilityToggle(ep.wire)) : []),
    [vendor],
  );

  // --- Catalog selection (per wire+model checkbox) + utility toggles, seeded from
  // the saved provider (a utility wire is on iff the provider already has it). ---
  const initialChecked = useMemo(() => {
    const s = new Set<string>();
    const haveWire = new Set(editing.endpoints.map((e) => e.wire));
    const haveModel = new Set(editing.models.map((m) => m.model));
    if (catalogVendor) {
      for (const ep of catalogVendor.endpoints) {
        if (!wireServesModels(ep.wire) || !haveWire.has(ep.wire)) continue;
        for (const m of ep.models ?? []) if (haveModel.has(m)) s.add(mkey(ep.wire, m));
      }
    }
    for (const ep of vendor?.endpoints ?? []) {
      if (wireIsUtilityToggle(ep.wire) && haveWire.has(ep.wire)) s.add(mkey(ep.wire, ''));
    }
    return s;
  }, [catalogVendor, vendor, editing]);
  const [selected, setSelected] = useState<Set<string> | null>(null);
  const checked = selected ?? initialChecked;
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

  // --- Custom state: base URL + free-form model rows, recovered from the provider ---
  const derivedBase = useMemo(
    () => (customVendor ? recoverBase(editing.endpoints, customVendor) : ''),
    [customVendor, editing.endpoints],
  );
  const derivedModels = useMemo(() => {
    const map: Record<string, string[]> = {};
    if (!isCustom) return map;
    const ids = editing.models.map((m) => m.model);
    for (const ep of modelWires) {
      if (editing.endpoints.some((e) => e.wire === ep.wire)) map[ep.wire] = [...ids];
    }
    return map;
  }, [isCustom, modelWires, editing]);
  const [baseUrl, setBaseUrl] = useState<string | null>(null);
  const baseValue = baseUrl ?? derivedBase;
  const [customModels, setCustomModels] = useState<Record<string, string[]> | null>(null);
  const customValue = customModels ?? derivedModels;
  const rowsFor = (wire: string) => customValue[wire] ?? [];
  const mutateRows = (wire: string, fn: (rows: string[]) => string[]) =>
    setCustomModels({ ...customValue, [wire]: fn(customValue[wire] ?? []) });
  const addModelRow = (wire: string) => mutateRows(wire, (r) => [...r, '']);
  const setModelRow = (wire: string, i: number, value: string) =>
    mutateRows(wire, (r) => r.map((m, idx) => (idx === i ? value : m)));
  const removeModelRow = (wire: string, i: number) =>
    mutateRows(wire, (r) => r.filter((_, idx) => idx !== i));

  // --- Per-model price overrides, seeded from the saved provider ---
  const [priceMap, setPriceMap] = useState<Record<string, CatalogPrice>>(() => {
    const m: Record<string, CatalogPrice> = {};
    for (const pm of editing.models) {
      if (pm.price_override) {
        m[pm.model] = {
          input: pm.input,
          output: pm.output,
          cached_input: pm.cached_input,
          unit: pm.unit,
          price_override: true,
        };
      }
    }
    return m;
  });
  const savedPriceMap = useMemo(() => {
    const m: Record<string, CatalogPrice> = {};
    for (const pm of editing.models) {
      m[pm.model] = {
        input: pm.input,
        output: pm.output,
        cached_input: pm.cached_input,
        unit: pm.unit,
        price_override: pm.price_override,
      };
    }
    return m;
  }, [editing.models]);
  const setPrice = (id: string, patch: Partial<CatalogPrice>) =>
    setPriceMap((p) => ({ ...p, [id]: { ...priceFor(id), ...patch, price_override: true } }));

  // Price used for a model: an explicit override wins, else the catalog/borrowed
  // price, else zero.
  const priceFor = (id: string): CatalogPrice => {
    if (priceMap[id]) return priceMap[id];
    const fromVendor = catalogVendor?.models[id];
    if (fromVendor)
      return {
        input: fromVendor.input,
        output: fromVendor.output,
        cached_input: fromVendor.cached_input ?? 0,
        unit: fromVendor.unit,
      };
    const borrowed = priceIndex[id];
    return borrowed ?? savedPriceMap[id] ?? { input: 0, output: 0, cached_input: 0, unit: 'per_1m_tokens' };
  };

  // --- Routing / behaviour knobs ---
  const [name, setName] = useState(editing.name);
  const [apiKey, setApiKey] = useState('');
  const [priority, setPriority] = useState(String(editing.priority));
  const [weight, setWeight] = useState(String(editing.weight));
  const [enabled, setEnabled] = useState(editing.enabled);
  const [allowUnmatched, setAllowUnmatched] = useState(editing.allow_unmatched);
  const [quirks, setQuirks] = useState<Record<string, string>>(editing.quirks ?? {});
  const injectStreamUsage = quirks['inject_stream_usage'] === 'true';
  const setInjectStreamUsage = (on: boolean) =>
    setQuirks((q) => {
      const next = { ...q };
      if (on) next['inject_stream_usage'] = 'true';
      else delete next['inject_stream_usage'];
      return next;
    });

  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);

  // The model ids currently active, in card order — drives the price table below.
  const activeModels = useMemo(() => {
    const ids: string[] = [];
    const seen = new Set<string>();
    const push = (id: string) => {
      const m = id.trim();
      if (m && !seen.has(m)) {
        seen.add(m);
        ids.push(m);
      }
    };
    for (const ep of modelWires) {
      if (isCustom) for (const m of customValue[ep.wire] ?? []) push(m);
      else for (const m of ep.models ?? []) if (checked.has(mkey(ep.wire, m))) push(m);
    }
    return ids;
  }, [modelWires, isCustom, customValue, checked]);

  if (catalog.error) return <ErrorBanner message={catalog.error} onRetry={catalog.refetch} />;
  if (catalog.initialLoading || !vendor)
    return (
      <div className={`card ${cards.card}`}>
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} height={22} style={{ marginBottom: 10 }} />
        ))}
      </div>
    );

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;

    const trimmedName = name.trim();
    if (!trimmedName) {
      setErr('Name is required.');
      return;
    }

    let base: string | null = null;
    if (isCustom) {
      base = baseValue.trim();
      try {
        const u = new URL(base);
        if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error('scheme');
      } catch {
        setErr('Base URL must be an absolute http(s) URL.');
        return;
      }
    }

    const wireModels = new Map<string, string[]>();
    for (const ep of modelWires) {
      if (isCustom) {
        const seen = new Set<string>();
        const ids: string[] = [];
        for (const raw of customValue[ep.wire] ?? []) {
          const m = raw.trim();
          if (!m || seen.has(m)) continue;
          seen.add(m);
          ids.push(m);
        }
        if (ids.length) wireModels.set(ep.wire, ids);
      } else {
        const picked = (ep.models ?? []).filter((m) => checked.has(mkey(ep.wire, m)));
        if (picked.length) wireModels.set(ep.wire, picked);
      }
    }

    const { endpoints, models } = buildProvider(
      vendor,
      wireModels,
      (id) => {
        const price = priceFor(id);
        return { model: id, ...price, price_override: price.price_override };
      },
      base,
      enabledUtility(),
    );
    if (models.length === 0) {
      setErr(isCustom ? 'Add at least one model.' : 'Select at least one model.');
      return;
    }

    const prio = Number(priority || '0');
    const wt = Number(weight || '1');
    if (Number.isNaN(prio) || Number.isNaN(wt)) {
      setErr('Priority and weight must be numbers.');
      return;
    }

    setBusy(true);
    setErr(null);
    try {
      const body: PatchProviderBody = {
        name: trimmedName,
        vendor: editing.vendor,
        priority: prio,
        weight: wt,
        enabled,
        allow_unmatched: allowUnmatched,
        quirks,
        models,
        endpoints,
      };
      if (apiKey.trim()) body.api_key = apiKey.trim();
      const saved = await api.patchProvider(editing.id, body);
      onSaved(saved);
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  const onDelete = async () => {
    if (deleting) return;
    setDeleting(true);
    setErr(null);
    try {
      await api.deleteProvider(editing.id);
      onDeleted?.();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Delete failed.');
      setConfirmingDelete(false);
      setDeleting(false);
    }
  };

  return (
    <form className={`card ${cards.card}`} onSubmit={submit}>
      <div className={cards.vendorHead}>
        <BrandIcon
          brand={providerBrand(editing.vendor || vendor.name, editing.models.map((m) => m.model))}
          label={editing.vendor || vendor.name}
          size={24}
        />
        <span className={cards.vendorName}>{editing.name}</span>
      </div>

      <div className={cards.field}>
        <label className={cards.label} htmlFor="s-name">
          Name
        </label>
        <input
          id="s-name"
          className={`input ${cards.keyInput}`}
          value={name}
          autoFocus
          placeholder="e.g. openai-main"
          onChange={(e) => setName(e.target.value)}
        />
        <span className={cards.hint}>Unique display name, shown in stats and the call ledger.</span>
      </div>

      <div className={cards.field}>
        <label className={cards.label} htmlFor="s-key">
          API key
        </label>
        <input
          id="s-key"
          className={`input mono ${cards.keyInput}`}
          type="password"
          value={apiKey}
          placeholder={
            editing.masked_key
              ? `${editing.masked_key} — leave blank to keep`
              : 'No key set — paste one to start routing'
          }
          onChange={(e) => setApiKey(e.target.value)}
        />
      </div>

      {isCustom && (
        <div className={cards.field}>
          <label className={cards.label} htmlFor="s-base">
            Base URL
          </label>
          <input
            id="s-base"
            className={`input mono ${cards.keyInput}`}
            value={baseValue}
            placeholder="https://your-endpoint.example"
            onChange={(e) => setBaseUrl(e.target.value)}
          />
          <span className={cards.hint}>
            The bare host; each wire's full path is appended (e.g.{' '}
            <code>{resolveEndpoint('{base}/v1/chat/completions', baseValue.trim() || '…')}</code>).
          </span>
        </div>
      )}

      <div className={cards.field}>
        <span className={cards.label}>APIs</span>
        {isCustom && (
          <span className={cards.hint}>
            Add models under the API(s) your endpoint serves; empty APIs are skipped.
          </span>
        )}
        <div className={cards.apiGrid}>
          {modelWires.map((ep) => (isCustom ? renderCustomCard(ep) : renderCatalogCard(ep)))}
          {utilityWires.map((ep) => renderUtilityCard(ep))}
        </div>
      </div>

      <div className={styles.divider} />

      <div className={styles.grid3}>
        <div className={cards.field}>
          <label className={cards.label} htmlFor="s-prio">
            Priority
          </label>
          <input
            id="s-prio"
            className="input"
            inputMode="numeric"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
          />
          <span className={cards.hint}>Lower = preferred.</span>
        </div>
        <div className={cards.field}>
          <label className={cards.label} htmlFor="s-weight">
            Weight
          </label>
          <input
            id="s-weight"
            className="input"
            inputMode="numeric"
            value={weight}
            onChange={(e) => setWeight(e.target.value)}
          />
          <span className={cards.hint}>Within a priority.</span>
        </div>
        <div className={cards.field}>
          <span className={cards.label}>Enabled</span>
          <label className={styles.toggleRow}>
            <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            <span>{enabled ? 'Routing' : 'Disabled'}</span>
          </label>
        </div>
      </div>

      <div className={styles.grid2}>
        <div className={cards.field}>
          <span className={cards.label}>Unmatched paths</span>
          <label className={styles.toggleRow}>
            <input
              type="checkbox"
              checked={allowUnmatched}
              onChange={(e) => setAllowUnmatched(e.target.checked)}
            />
            <span>{allowUnmatched ? 'Forward (metered zero)' : 'Deny (recommended)'}</span>
          </label>
          <span className={cards.hint}>
            Opt-in passthrough for endpoints without a wire — spend on them is not metered.
          </span>
        </div>
        <div className={cards.field}>
          <span className={cards.label}>Stream usage injection</span>
          <label className={styles.toggleRow}>
            <input
              type="checkbox"
              checked={injectStreamUsage}
              onChange={(e) => setInjectStreamUsage(e.target.checked)}
            />
            <span>{injectStreamUsage ? 'Inject include_usage' : 'Off (bodies untouched)'}</span>
          </label>
          <span className={cards.hint}>
            Some vendors omit usage from streams unless asked; this sets
            stream_options.include_usage on streamed requests.
          </span>
        </div>
      </div>

      <div className={cards.field}>
        <span className={styles.advTitle}>Model prices</span>
        <span className={cards.hint}>
          Per-unit rates used for metering. &quot;Cached in&quot; is the rate for cache-hit input
          tokens; 0 = full input rate. Add or remove models from the API cards above.
        </span>
        {activeModels.length === 0 ? (
          <span className="muted" style={{ fontSize: 12.5 }}>
            No models selected yet.
          </span>
        ) : (
          <div className={styles.modelRows}>
            <div className={`${styles.modelRow} ${styles.modelHeader}`}>
              <span>Model</span>
              <span>Input</span>
              <span>Output</span>
              <span>Cached in</span>
              <span>Unit</span>
              <span />
            </div>
            {activeModels.map((id) => {
              const p = priceFor(id);
              return (
                <div key={id} className={styles.modelRow}>
                  <span className="mono" style={{ fontSize: 12.5, wordBreak: 'break-all' }}>
                    {id}
                  </span>
                  <input
                    className="input"
                    inputMode="decimal"
                    value={String(p.input)}
                    onChange={(e) => setPrice(id, { input: Number(e.target.value || '0') })}
                  />
                  <input
                    className="input"
                    inputMode="decimal"
                    value={String(p.output)}
                    onChange={(e) => setPrice(id, { output: Number(e.target.value || '0') })}
                  />
                  <input
                    className="input"
                    inputMode="decimal"
                    value={String(p.cached_input)}
                    onChange={(e) => setPrice(id, { cached_input: Number(e.target.value || '0') })}
                  />
                  <select
                    className="select"
                    value={p.unit}
                    onChange={(e) => setPrice(id, { unit: e.target.value })}
                  >
                    {UNITS.map((u) => (
                      <option key={u} value={u}>
                        {u}
                      </option>
                    ))}
                  </select>
                  <span />
                </div>
              );
            })}
          </div>
        )}
      </div>

      {err && <div className={cards.error}>{err}</div>}

      <div className={styles.footerRow}>
        {onDeleted && (
          <div className={styles.footerLeft}>
            {confirmingDelete ? (
              <>
                <button
                  type="button"
                  className="btn btn-danger"
                  disabled={deleting}
                  title="Removes this provider and its endpoints, models, and routing. This cannot be undone."
                  onClick={onDelete}
                >
                  {deleting ? 'Deleting…' : 'Confirm delete'}
                </button>
                <button
                  type="button"
                  className="btn"
                  disabled={deleting}
                  onClick={() => setConfirmingDelete(false)}
                >
                  Cancel
                </button>
              </>
            ) : (
              <button
                type="button"
                className="btn btn-danger"
                disabled={busy}
                onClick={() => setConfirmingDelete(true)}
              >
                <Trash2 size={14} /> Delete
              </button>
            )}
          </div>
        )}
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? 'Saving…' : 'Save changes'}
        </button>
      </div>
    </form>
  );

  // Catalog card: checkboxes over the preset's models.
  function renderCatalogCard(ep: CatalogEndpoint) {
    const models = ep.models ?? [];
    const keys = models.map((m) => mkey(ep.wire, m));
    const onCount = keys.filter((k) => checked.has(k)).length;
    const allOn = onCount === keys.length;
    const someOn = onCount > 0;
    return (
      <div key={ep.wire} className={`card ${cards.apiCard} ${someOn ? cards.apiCardOn : ''}`}>
        <button
          type="button"
          className={cards.apiCardHead}
          aria-pressed={allOn}
          onClick={() => toggleWire(ep)}
        >
          <span className={cards.apiName}>{wireName(ep.wire, vendor!.id)}</span>
          <span className={`${cards.apiCheck} ${someOn ? cards.apiCheckOn : ''}`}>
            {allOn ? <Check size={13} strokeWidth={3} /> : someOn ? <Minus size={13} strokeWidth={3} /> : null}
          </span>
        </button>
        <span className={cards.apiUrl}>{ep.endpoint}</span>
        <div className={cards.modelList}>
          {models.map((m) => {
            const on = checked.has(mkey(ep.wire, m));
            return (
              <label key={m} className={cards.modelRow}>
                <span className={cards.modelName}>{m}</span>
                <input
                  type="checkbox"
                  className={cards.srOnly}
                  checked={on}
                  onChange={() => toggleModel(ep.wire, m)}
                />
                <span className={`${cards.modelCheck} ${on ? cards.apiCheckOn : ''}`}>
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
    const resolved = resolveEndpoint(ep.endpoint, baseValue.trim() || '…');
    return (
      <div key={ep.wire} className={`card ${cards.apiCard} ${filled ? cards.apiCardOn : ''}`}>
        <div className={cards.apiCardHead}>
          <span className={cards.apiName}>{wireName(ep.wire, vendor!.id)}</span>
        </div>
        <span className={cards.apiUrl}>{resolved}</span>
        <div className={cards.modelList}>
          {rows.map((m, i) => {
            const price = priceIndex[m.trim()];
            return (
              <div key={i} className={cards.modelInputRow}>
                <input
                  className={`input mono ${cards.modelInput}`}
                  value={m}
                  placeholder="model-id"
                  onChange={(e) => setModelRow(ep.wire, i, e.target.value)}
                />
                <span className={cards.modelPrice}>
                  {m.trim() ? (price ? `in ${price.input} · out ${price.output}` : 'unpriced') : ''}
                </span>
                <button
                  type="button"
                  className={cards.iconBtn}
                  aria-label="Remove model"
                  onClick={() => removeModelRow(ep.wire, i)}
                >
                  <Trash2 size={13} />
                </button>
              </div>
            );
          })}
          <button type="button" className={cards.addBtn} onClick={() => addModelRow(ep.wire)}>
            <Plus size={13} /> Add model
          </button>
        </div>
      </div>
    );
  }

  // Utility card: a model-less wire (e.g. count_tokens) toggled on/off as a whole.
  function renderUtilityCard(ep: CatalogEndpoint) {
    const on = checked.has(mkey(ep.wire, ''));
    const url = resolveEndpoint(ep.endpoint, isCustom ? baseValue.trim() || '…' : null);
    return (
      <div key={ep.wire} className={`card ${cards.apiCard} ${on ? cards.apiCardOn : ''}`}>
        <button
          type="button"
          className={cards.apiCardHead}
          aria-pressed={on}
          onClick={() => toggleUtility(ep.wire)}
        >
          <span className={cards.apiName}>{wireName(ep.wire, vendor!.id)}</span>
          <span className={`${cards.apiCheck} ${on ? cards.apiCheckOn : ''}`}>
            {on ? <Check size={13} strokeWidth={3} /> : null}
          </span>
        </button>
        <span className={cards.apiUrl}>{url}</span>
        <div className={cards.utilityNote}>{wireUtilityNote(ep.wire)}</div>
      </div>
    );
  }
}

// recoverBase reverses a custom provider's saved endpoints back to the shared base
// URL it was built from: for each endpoint matching a template wire, strip the
// template's post-{base} suffix. Prefers a model-serving wire with a real suffix
// (e.g. /chat/completions) over the bare {base} model-listing wire.
function recoverBase(endpoints: ProviderEndpoint[], template: CatalogVendor): string {
  let fallback = '';
  for (const pe of endpoints) {
    const te = template.endpoints.find((t) => t.wire === pe.wire);
    if (!te) continue;
    const suffix = te.endpoint.replace('{base}', '');
    if (!suffix) {
      fallback ||= pe.endpoint.replace(/\/+$/, '');
      continue;
    }
    if (pe.endpoint.endsWith(suffix)) return pe.endpoint.slice(0, -suffix.length);
  }
  return fallback;
}
