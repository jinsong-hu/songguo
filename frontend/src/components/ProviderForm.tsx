import { useState, type FormEvent } from 'react';
import { Plus, Trash2 } from 'lucide-react';
import { api } from '../api/client';
import type { CreateProviderBody, PatchProviderBody, Provider, ProviderModel } from '../api/types';
import { useFetch } from '../lib/useFetch';
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

const ADAPTERS = [
  { value: 'openai-compatible', label: 'OpenAI-compatible' },
  { value: 'anthropic-compatible', label: 'Anthropic-compatible' },
  { value: 'mcp', label: 'MCP (listed only)' },
];

/** Wire allowlist granted when the user picked none, mirroring the backend. */
function defaultWires(adapter: string): string[] {
  if (adapter === 'anthropic-compatible') return ['anthropic/messages', 'anthropic/models'];
  return ['openai/chat', 'openai/completions', 'openai/embeddings', 'openai/models'];
}

/** Prefill seeds the form when adding from the catalog. */
export interface ProviderPrefill {
  name?: string;
  vendor?: string;
  adapter?: string;
  base_url?: string;
  catalog_id?: string;
  wires?: string[];
  quirks?: Record<string, string>;
  models?: ProviderModel[];
}

interface ProviderFormProps {
  editing?: Provider;
  prefill?: ProviderPrefill;
  onCancel: () => void;
  onSaved: (provider: Provider, created: boolean) => void;
}

/** Editable model row keeps numbers as strings so fields can be cleared. */
interface ModelRow {
  model: string;
  input: string;
  output: string;
  cachedInput: string;
  unit: string;
}

function toRows(models: ProviderModel[] | undefined): ModelRow[] {
  if (!models || models.length === 0) return [];
  return models.map((m) => ({
    model: m.model,
    input: String(m.input ?? 0),
    output: String(m.output ?? 0),
    cachedInput: String(m.cached_input ?? 0),
    unit: m.unit || 'per_1m_tokens',
  }));
}

export function ProviderForm({ editing, prefill, onCancel, onSaved }: ProviderFormProps) {
  const seed = editing ?? prefill;
  const [name, setName] = useState(seed?.name ?? '');
  const [vendor] = useState(seed?.vendor ?? '');
  const [adapter, setAdapter] = useState(seed?.adapter ?? 'openai-compatible');
  const [baseUrl, setBaseUrl] = useState(seed?.base_url ?? '');
  const [priority, setPriority] = useState(editing ? String(editing.priority) : '0');
  const [weight, setWeight] = useState(editing ? String(editing.weight) : '1');
  const [enabled, setEnabled] = useState(editing ? editing.enabled : true);
  const [apiKey, setApiKey] = useState('');
  const [models, setModels] = useState<ModelRow[]>(toRows(seed?.models));
  const [wires, setWires] = useState<string[]>(
    seed?.wires?.length ? seed.wires : defaultWires(seed?.adapter ?? 'openai-compatible'),
  );
  const [allowUnmatched, setAllowUnmatched] = useState(editing?.allow_unmatched ?? false);
  const [quirks, setQuirks] = useState<Record<string, string>>(seed?.quirks ?? {});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const allWires = useFetch(() => api.wires(), []);
  const wireOptions = allWires.data ?? [];

  const isEdit = !!editing;
  const catalogId = editing?.catalog_id ?? prefill?.catalog_id ?? '';

  const toggleWire = (w: string) =>
    setWires((p) => (p.includes(w) ? p.filter((x) => x !== w) : [...p, w].sort()));

  const injectStreamUsage = quirks['inject_stream_usage'] === 'true';
  const setInjectStreamUsage = (on: boolean) =>
    setQuirks((q) => {
      const next = { ...q };
      if (on) next['inject_stream_usage'] = 'true';
      else delete next['inject_stream_usage'];
      return next;
    });

  const addModel = () =>
    setModels((p) => [
      ...p,
      { model: '', input: '0', output: '0', cachedInput: '0', unit: 'per_1m_tokens' },
    ]);
  const removeModel = (i: number) => setModels((p) => p.filter((_, idx) => idx !== i));
  const setModel = (i: number, patch: Partial<ModelRow>) =>
    setModels((p) => p.map((row, idx) => (idx === i ? { ...row, ...patch } : row)));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;

    const trimmedName = name.trim();
    if (!trimmedName) {
      setErr('Name is required.');
      return;
    }
    const trimmedUrl = baseUrl.trim();
    if (!trimmedUrl) {
      setErr('Base URL is required.');
      return;
    }
    try {
      const u = new URL(trimmedUrl);
      if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error('scheme');
    } catch {
      setErr('Base URL must be an absolute http(s) URL, e.g. https://api.openai.com/v1');
      return;
    }

    const parsedModels: ProviderModel[] = [];
    for (const row of models) {
      const m = row.model.trim();
      if (!m) continue;
      const input = Number(row.input || '0');
      const output = Number(row.output || '0');
      const cachedInput = Number(row.cachedInput || '0');
      if (
        Number.isNaN(input) ||
        Number.isNaN(output) ||
        Number.isNaN(cachedInput) ||
        input < 0 ||
        output < 0 ||
        cachedInput < 0
      ) {
        setErr(`Price for "${m}" must be non-negative numbers.`);
        return;
      }
      parsedModels.push({ model: m, input, output, cached_input: cachedInput, unit: row.unit });
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
      if (isEdit && editing) {
        const body: PatchProviderBody = {
          name: trimmedName,
          vendor,
          adapter,
          base_url: trimmedUrl,
          priority: prio,
          weight: wt,
          enabled,
          allow_unmatched: allowUnmatched,
          quirks,
          models: parsedModels,
          wires,
        };
        if (apiKey.trim()) body.api_key = apiKey.trim();
        const saved = await api.patchProvider(editing.id, body);
        onSaved(saved, false);
      } else {
        const body: CreateProviderBody = {
          name: trimmedName,
          vendor,
          adapter,
          base_url: trimmedUrl,
          priority: prio,
          weight: wt,
          enabled,
          catalog_id: catalogId || undefined,
          allow_unmatched: allowUnmatched,
          quirks,
          api_key: apiKey.trim() || undefined,
          models: parsedModels,
          wires,
        };
        const saved = await api.createProvider(body);
        onSaved(saved, true);
      }
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <form className={`card ${styles.formCard}`} onSubmit={submit}>
      <div className={styles.grid2}>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="s-name">
            Name
          </label>
          <input
            id="s-name"
            className="input"
            value={name}
            autoFocus
            placeholder="e.g. openai-main"
            onChange={(e) => setName(e.target.value)}
          />
          <span className={styles.hint}>Unique handle; also addressable at /x/&lt;name&gt;/…</span>
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="s-adapter">
            Adapter
          </label>
          <select
            id="s-adapter"
            className="select"
            value={adapter}
            onChange={(e) => setAdapter(e.target.value)}
          >
            {ADAPTERS.map((a) => (
              <option key={a.value} value={a.value}>
                {a.label}
              </option>
            ))}
          </select>
          <span className={styles.hint}>Wire protocol used to authenticate + meter.</span>
        </div>
      </div>

      <div className={styles.field}>
        <label className={styles.label} htmlFor="s-url">
          Base URL
        </label>
        <input
          id="s-url"
          className="input"
          value={baseUrl}
          placeholder="https://api.openai.com/v1"
          onChange={(e) => setBaseUrl(e.target.value)}
        />
        <span className={styles.hint}>
          The vendor&apos;s published base, including any version prefix (e.g. /v1, /api/v3).
        </span>
      </div>

      <div className={styles.field}>
        <label className={styles.label} htmlFor="s-key">
          API key
        </label>
        <input
          id="s-key"
          className="input mono"
          type="password"
          value={apiKey}
          placeholder={
            isEdit
              ? editing?.masked_key
                ? `${editing.masked_key} — leave blank to keep`
                : 'No key set — paste one to start routing'
              : 'sk-…'
          }
          onChange={(e) => setApiKey(e.target.value)}
        />
        <span className={styles.hint}>
          One key per provider. Stored as-is; shown masked afterwards.
        </span>
      </div>

      <div className={styles.grid3}>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="s-prio">
            Priority
          </label>
          <input
            id="s-prio"
            className="input"
            inputMode="numeric"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
          />
          <span className={styles.hint}>Lower = preferred.</span>
        </div>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="s-weight">
            Weight
          </label>
          <input
            id="s-weight"
            className="input"
            inputMode="numeric"
            value={weight}
            onChange={(e) => setWeight(e.target.value)}
          />
          <span className={styles.hint}>Within a priority.</span>
        </div>
        <div className={styles.field}>
          <span className={styles.label}>Enabled</span>
          <label className={styles.toggleRow}>
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            <span>{enabled ? 'Routing' : 'Disabled'}</span>
          </label>
        </div>
      </div>

      <div className={styles.field}>
        <span className={styles.label}>Wires</span>
        <span className={styles.hint}>
          Endpoints the proxy may serve for this service. Requests matching no enabled
          wire are denied (so every forwarded call has a pricing rule).
        </span>
        <div className={styles.wireGrid}>
          {wireOptions.map((w) => (
            <label key={w} className={styles.wireItem}>
              <input type="checkbox" checked={wires.includes(w)} onChange={() => toggleWire(w)} />
              <span className="mono">{w}</span>
            </label>
          ))}
        </div>
      </div>

      <div className={styles.grid2}>
        <div className={styles.field}>
          <span className={styles.label}>Unmatched paths</span>
          <label className={styles.toggleRow}>
            <input
              type="checkbox"
              checked={allowUnmatched}
              onChange={(e) => setAllowUnmatched(e.target.checked)}
            />
            <span>{allowUnmatched ? 'Forward (metered zero)' : 'Deny (recommended)'}</span>
          </label>
          <span className={styles.hint}>
            Opt-in passthrough for endpoints without a wire — spend on them is not metered.
          </span>
        </div>
        <div className={styles.field}>
          <span className={styles.label}>Stream usage injection</span>
          <label className={styles.toggleRow}>
            <input
              type="checkbox"
              checked={injectStreamUsage}
              onChange={(e) => setInjectStreamUsage(e.target.checked)}
            />
            <span>{injectStreamUsage ? 'Inject include_usage' : 'Off (bodies untouched)'}</span>
          </label>
          <span className={styles.hint}>
            Some vendors omit usage from streams unless asked; this sets
            stream_options.include_usage on streamed requests.
          </span>
        </div>
      </div>

      <div className={styles.field}>
        <div className={styles.modelsHead}>
          <span className={styles.label}>Models &amp; prices</span>
          <button type="button" className="btn btn-sm" onClick={addModel}>
            <Plus size={13} /> Add model
          </button>
        </div>
        <span className={styles.hint}>
          Each row is a served model with its true per-unit price (used for metering).
          &quot;Cached in&quot; is the rate for cache-hit input tokens; 0 = full input rate.
        </span>
        {models.length === 0 ? (
          <span className="muted" style={{ fontSize: 12.5 }}>
            No models yet — a service with no models is saved as a draft and won&apos;t route
            until you add one.
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
            {models.map((row, i) => (
              <div key={i} className={styles.modelRow}>
                <input
                  className="input mono"
                  value={row.model}
                  placeholder="gpt-4o"
                  onChange={(e) => setModel(i, { model: e.target.value })}
                />
                <input
                  className="input"
                  inputMode="decimal"
                  value={row.input}
                  onChange={(e) => setModel(i, { input: e.target.value })}
                />
                <input
                  className="input"
                  inputMode="decimal"
                  value={row.output}
                  onChange={(e) => setModel(i, { output: e.target.value })}
                />
                <input
                  className="input"
                  inputMode="decimal"
                  value={row.cachedInput}
                  onChange={(e) => setModel(i, { cachedInput: e.target.value })}
                />
                <select
                  className="select"
                  value={row.unit}
                  onChange={(e) => setModel(i, { unit: e.target.value })}
                >
                  {UNITS.map((u) => (
                    <option key={u} value={u}>
                      {u}
                    </option>
                  ))}
                </select>
                <button
                  type="button"
                  className={styles.iconBtn}
                  aria-label="Remove model"
                  onClick={() => removeModel(i)}
                >
                  <Trash2 size={14} />
                </button>
              </div>
            ))}
          </div>
        )}
      </div>

      {err && <div className={styles.error}>{err}</div>}

      <div className={styles.footerRow}>
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? 'Saving…' : isEdit ? 'Save changes' : 'Add provider'}
        </button>
      </div>
    </form>
  );
}
