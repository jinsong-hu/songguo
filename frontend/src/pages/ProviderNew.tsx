import { useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Plus, Trash2 } from 'lucide-react';
import { api } from '../api/client';
import type { Catalog, CreateProviderBody, ProviderEndpoint, ProviderModel } from '../api/types';
import { Page } from '../components/Layout';
import { useToast } from '../components/Toast';
import { useFetch } from '../lib/useFetch';
import { wireAdapter, wireName, wireServesModels } from '../lib/wires';
import styles from '../components/ProviderForm.module.css';

// The three custom kinds. `wire` is the fixed model-serving wire; `null` means
// the user picks the protocol ("Custom Any"). The base URL is shared by every
// endpoint, so the user only types it once.
const KIND_META: Record<
  string,
  { title: string; wire: string | null; urlPlaceholder: string; modelPlaceholder: string }
> = {
  openai: {
    title: 'OpenAI',
    wire: 'openai/chat',
    urlPlaceholder: 'https://api.openai.com/v1',
    modelPlaceholder: 'gpt-5.4',
  },
  anthropic: {
    title: 'Anthropic',
    wire: 'anthropic/messages',
    urlPlaceholder: 'https://api.anthropic.com/v1',
    modelPlaceholder: 'claude-sonnet-4-6',
  },
  any: {
    title: 'Any',
    wire: null,
    urlPlaceholder: 'https://your-endpoint.example/v1',
    modelPlaceholder: 'model-id',
  },
};

// Path appended to the user's base URL to form each wire's full upstream URL.
// Model-listing wires (openai/models, anthropic/models) and volc/* speech wires
// hit the base as-is (suffix "").
const WIRE_SUFFIX: Record<string, string> = {
  'openai/chat': '/chat/completions',
  'openai/responses': '/responses',
  'openai/embeddings': '/embeddings',
  'openai/completions': '/completions',
  'openai/images': '/images/generations',
  'openai/models': '',
  'anthropic/messages': '/messages',
  'anthropic/models': '',
  'ark/video': '/contents/generations/tasks',
};

// Join a base URL with a wire's path suffix to get the full upstream endpoint.
function fullEndpoint(wire: string, base: string): string {
  return base.replace(/\/+$/, '') + (WIRE_SUFFIX[wire] ?? '');
}

/** Per-model price as carried in the embedded catalog (used for metering). */
interface CatalogPrice {
  input: number;
  output: number;
  cached_input: number;
  unit: string;
}

// Flatten every catalog vendor's price list into one model-id → price index.
// The first vendor to define a model id wins; custom providers borrow these
// prices so the user never re-enters them.
function buildPriceIndex(catalog: Catalog | null | undefined): Record<string, CatalogPrice> {
  const index: Record<string, CatalogPrice> = {};
  for (const vendor of catalog?.vendors ?? []) {
    for (const [id, m] of Object.entries(vendor.models)) {
      if (index[id]) continue;
      index[id] = { input: m.input, output: m.output, cached_input: m.cached_input ?? 0, unit: m.unit };
    }
  }
  return index;
}

export function ProviderNewPage() {
  const { kind = 'openai' } = useParams<{ kind: string }>();
  const meta = KIND_META[kind] ?? KIND_META.openai;
  const navigate = useNavigate();
  const toast = useToast();

  const catalog = useFetch(() => api.catalog(), []);
  const allWires = useFetch(() => api.wires(), []);
  const priceIndex = useMemo(() => buildPriceIndex(catalog.data), [catalog.data]);
  const protocolOptions = useMemo(
    () => (allWires.data ?? []).filter(wireServesModels),
    [allWires.data],
  );

  const [name, setName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const [wire, setWire] = useState('openai/chat');
  const [models, setModels] = useState<string[]>(['']);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const activeWire = meta.wire ?? wire;

  const setModelAt = (i: number, value: string) =>
    setModels((p) => p.map((m, idx) => (idx === i ? value : m)));
  const addModel = () => setModels((p) => [...p, '']);
  const removeModel = (i: number) =>
    setModels((p) => (p.length === 1 ? [''] : p.filter((_, idx) => idx !== i)));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;

    const trimmedName = name.trim();
    if (!trimmedName) {
      setErr('Name is required.');
      return;
    }
    const url = baseUrl.trim();
    try {
      const u = new URL(url);
      if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error('scheme');
    } catch {
      setErr('Base URL must be an absolute http(s) URL.');
      return;
    }

    const adapter = wireAdapter(activeWire);
    const endpoints: ProviderEndpoint[] = [
      { wire: activeWire, endpoint: fullEndpoint(activeWire, url), adapter },
    ];
    // OpenAI/Anthropic also expose a model-listing wire at the same base; add it
    // automatically so tools that call /models keep working.
    const prefix = activeWire.split('/')[0];
    const listWire = `${prefix}/models`;
    if ((prefix === 'openai' || prefix === 'anthropic') && listWire !== activeWire) {
      endpoints.push({ wire: listWire, endpoint: fullEndpoint(listWire, url), adapter });
    }

    const parsedModels: ProviderModel[] = [];
    const seen = new Set<string>();
    for (const raw of models) {
      const m = raw.trim();
      if (!m || seen.has(m)) continue;
      seen.add(m);
      const price = priceIndex[m];
      parsedModels.push({
        model: m,
        input: price?.input ?? 0,
        output: price?.output ?? 0,
        cached_input: price?.cached_input ?? 0,
        unit: price?.unit ?? 'per_1m_tokens',
      });
    }

    setBusy(true);
    setErr(null);
    try {
      const body: CreateProviderBody = {
        name: trimmedName,
        enabled: true,
        api_key: apiKey.trim() || undefined,
        models: parsedModels,
        endpoints,
      };
      await api.createProvider(body);
      toast.success('Provider added.');
      navigate('/providers');
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : 'Request failed.');
      setBusy(false);
    }
  };

  return (
    <Page
      title={`Add Custom ${meta.title}`}
      actions={
        <Link to="/providers" className="btn">
          <ArrowLeft size={15} /> Back
        </Link>
      }
    >
      <form className={`card ${styles.formCard}`} onSubmit={submit}>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="c-name">
            Name
          </label>
          <input
            id="c-name"
            className="input"
            value={name}
            autoFocus
            placeholder={`my-${kind}`}
            onChange={(e) => setName(e.target.value)}
          />
          <span className={styles.hint}>Unique display name, shown in stats and the call ledger</span>
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="c-key">
            API key
          </label>
          <input
            id="c-key"
            className="input mono"
            type="password"
            value={apiKey}
            placeholder="sk-…"
            onChange={(e) => setApiKey(e.target.value)}
          />
        </div>

        {meta.wire === null && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="c-wire">
              Protocol
            </label>
            <select
              id="c-wire"
              className="select"
              value={wire}
              onChange={(e) => setWire(e.target.value)}
            >
              {protocolOptions.map((w) => (
                <option key={w} value={w}>
                  {wireName(w)} — {w}
                </option>
              ))}
            </select>
            <span className={styles.hint}>The wire format this endpoint speaks.</span>
          </div>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="c-url">
            Base URL
          </label>
          <input
            id="c-url"
            className="input mono"
            value={baseUrl}
            placeholder={meta.urlPlaceholder}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
          <span className={styles.hint}>
            Auth scheme ({wireAdapter(activeWire)}) and the full upstream URL are derived from the
            protocol
            {baseUrl.trim() ? <> → <code>{fullEndpoint(activeWire, baseUrl.trim())}</code></> : '.'}
          </span>
        </div>

        <div className={styles.field}>
          <div className={styles.modelsHead}>
            <span className={styles.label}>Models</span>
            <button type="button" className="btn btn-sm" onClick={addModel}>
              <Plus size={13} /> Add model
            </button>
          </div>
          <span className={styles.hint}>
            Prices are matched from the built-in catalog by model id; unknown models meter at $0.
          </span>
          <div className={styles.modelRows}>
            {models.map((m, i) => {
              const price = priceIndex[m.trim()];
              return (
                <div
                  key={i}
                  style={{ display: 'flex', alignItems: 'center', gap: 8 }}
                >
                  <input
                    className="input mono"
                    style={{ flex: 1 }}
                    value={m}
                    placeholder={meta.modelPlaceholder}
                    onChange={(e) => setModelAt(i, e.target.value)}
                  />
                  <span
                    className="muted mono"
                    style={{ fontSize: 11.5, width: 130, textAlign: 'right' }}
                  >
                    {m.trim()
                      ? price
                        ? `in ${price.input} · out ${price.output}`
                        : 'unpriced'
                      : ''}
                  </span>
                  <button
                    type="button"
                    className={styles.iconBtn}
                    aria-label="Remove model"
                    onClick={() => removeModel(i)}
                  >
                    <Trash2 size={14} />
                  </button>
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
            {busy ? 'Saving…' : 'Add provider'}
          </button>
        </div>
      </form>
    </Page>
  );
}
