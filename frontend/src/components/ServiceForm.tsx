import { useState, type FormEvent } from 'react';
import { Plus, Trash2 } from 'lucide-react';
import { api } from '../api/client';
import type { CreateServiceBody, PatchServiceBody, Service, ServiceModel } from '../api/types';
import { Modal } from './Modal';
import styles from './ServiceForm.module.css';

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

/** Prefill seeds the form when adding from the catalog. */
export interface ServicePrefill {
  name?: string;
  vendor?: string;
  adapter?: string;
  base_url?: string;
  catalog_id?: string;
  models?: ServiceModel[];
}

interface ServiceFormProps {
  editing?: Service;
  prefill?: ServicePrefill;
  onClose: () => void;
  onSaved: (service: Service, created: boolean) => void;
}

/** Editable model row keeps numbers as strings so fields can be cleared. */
interface ModelRow {
  model: string;
  input: string;
  output: string;
  unit: string;
}

function toRows(models: ServiceModel[] | undefined): ModelRow[] {
  if (!models || models.length === 0) return [];
  return models.map((m) => ({
    model: m.model,
    input: String(m.input ?? 0),
    output: String(m.output ?? 0),
    unit: m.unit || 'per_1m_tokens',
  }));
}

export function ServiceForm({ editing, prefill, onClose, onSaved }: ServiceFormProps) {
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
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const isEdit = !!editing;
  const catalogId = editing?.catalog_id ?? prefill?.catalog_id ?? '';

  const addModel = () =>
    setModels((p) => [...p, { model: '', input: '0', output: '0', unit: 'per_1m_tokens' }]);
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

    const parsedModels: ServiceModel[] = [];
    for (const row of models) {
      const m = row.model.trim();
      if (!m) continue;
      const input = Number(row.input || '0');
      const output = Number(row.output || '0');
      if (Number.isNaN(input) || Number.isNaN(output) || input < 0 || output < 0) {
        setErr(`Price for "${m}" must be non-negative numbers.`);
        return;
      }
      parsedModels.push({ model: m, input, output, unit: row.unit });
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
        const body: PatchServiceBody = {
          name: trimmedName,
          vendor,
          adapter,
          base_url: trimmedUrl,
          priority: prio,
          weight: wt,
          enabled,
          models: parsedModels,
        };
        const saved = await api.patchService(editing.id, body);
        onSaved(saved, false);
      } else {
        const body: CreateServiceBody = {
          name: trimmedName,
          vendor,
          adapter,
          base_url: trimmedUrl,
          priority: prio,
          weight: wt,
          enabled,
          catalog_id: catalogId || undefined,
          api_keys: apiKey.trim() ? [apiKey.trim()] : [],
          models: parsedModels,
        };
        const saved = await api.createService(body);
        onSaved(saved, true);
      }
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <Modal
      title={isEdit ? `Edit ${editing!.name}` : 'Add service'}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="submit" form="service-form" className="btn btn-primary" disabled={busy}>
            {busy ? 'Saving…' : isEdit ? 'Save changes' : 'Add service'}
          </button>
        </>
      }
    >
      <form id="service-form" onSubmit={submit}>
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

        {!isEdit && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="s-key">
              API key
            </label>
            <input
              id="s-key"
              className="input mono"
              type="password"
              value={apiKey}
              placeholder="sk-…  (add more keys later to rotate the pool)"
              onChange={(e) => setApiKey(e.target.value)}
            />
            <span className={styles.hint}>Stored as-is; shown masked afterwards.</span>
          </div>
        )}

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
          <div className={styles.modelsHead}>
            <span className={styles.label}>Models &amp; prices</span>
            <button type="button" className="btn btn-sm" onClick={addModel}>
              <Plus size={13} /> Add model
            </button>
          </div>
          <span className={styles.hint}>
            Each row is a served model with its true per-unit price (used for metering).
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
      </form>
    </Modal>
  );
}
