import { useEffect, useMemo, useState } from 'react';
import { Download, Send } from 'lucide-react';
import type { Catalog, Provider, Service } from '../api/types';
import { getAdminKey } from '../api/client';
import { modelMeta } from '../lib/modelBrand';
import { wireFullName } from '../lib/wires';
import {
  buildTestRequest,
  codeTabsFor,
  DEFAULT_TTS_VOICE,
  runAsr,
  runImage,
  runTest,
  runTts,
  runVideo,
  wireNeedsProviderPin,
  wireTests,
  type AsrResult,
  type CodeTab,
  type ImageResult,
  type TestResult,
  type TtsResult,
  type VideoResult,
  type WireTest,
} from '../lib/playground';
import { int, ms } from '../lib/format';
import { CopyButton } from './CopyButton';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from './ui/select';
import styles from './Playground.module.css';

interface PlaygroundProps {
  /** Model directory: every testable model and which providers serve it. */
  services: Service[];
  /** Full provider objects (endpoints, models, names) for routing + wires. */
  providers: Provider[];
  /** Catalog, to know which wires actually serve which model. */
  catalog: Catalog | null;
  /** Host-seeded default model (changeable); falls back to the first model. */
  defaultModel?: string;
}

/** Routing sentinel for "let the gateway pick" (Radix Select forbids ""). */
const AUTO = '__auto__';

/** The providers (full objects) that serve a model, matched by id. */
function servingProviders(service: Service | undefined, providers: Provider[]): Provider[] {
  if (!service) return [];
  const ids = new Set(service.providers.map((p) => p.id));
  return providers.filter((p) => ids.has(p.id));
}

/** Wires that, per the catalog, actually serve a model (across all vendors). */
function wiresForModel(catalog: Catalog | null, model: string): Set<string> {
  const wires = new Set<string>();
  if (!catalog) return wires;
  for (const vendor of catalog.vendors) {
    for (const ep of vendor.endpoints) {
      if ((ep.models ?? []).includes(model)) wires.add(ep.wire);
    }
  }
  return wires;
}

/**
 * Wires enabled on the providers serving a model, narrowed to those the catalog
 * says actually serve it — a provider key may carry sibling wires (image, video,
 * ASR…) for other models, which this model's test must not offer. When the
 * catalog has nothing for the model (custom/off-catalog), fall back to the
 * provider's full wire set so there is still a request shape.
 */
function wiresOf(providers: Provider[], serving: Set<string>): string[] {
  const wires = new Set<string>();
  for (const p of providers) {
    for (const ep of p.endpoints) {
      if (serving.size === 0 || serving.has(ep.wire)) wires.add(ep.wire);
    }
  }
  return [...wires];
}

/** The proxy path for an endpoint option, sans method ("POST /v1/x" → "/v1/x").
 *  Always normalized to a leading "/"; a wire without a mapped endpoint shows
 *  its id under one ("/openai/images") rather than a bare, odd-one-out label. */
function endpointPath(test: WireTest): string {
  const path = test.endpoint.replace(/^[A-Z]+\s+/, '') || test.wire;
  return path.startsWith('/') ? path : `/${path}`;
}

/** A model's testable wires and the providers that serve each. */
interface ModelInfo {
  model: string;
  /** Testable wires this model exposes, in ranked order. */
  wires: string[];
  /** Servable providers per wire (those whose endpoint exposes the wire). */
  providersByWire: Map<string, Provider[]>;
}

/**
 * Interactive test card for any model the gateway serves. Self-contained: a host
 * page seeds the default model and the card derives everything else from the
 * services/providers/catalog it is given, so the same impl backs every service
 * page (and anywhere else a test card belongs).
 *
 * Each of the four selectors — Endpoint, Wire, Model, Routing — lists the full
 * universe for its axis in a stable static order, so nothing moves or hides as
 * you choose. Endpoint and Wire are two views of one axis (1:1: the proxy path
 * and the protocol it speaks). A pick may be incompatible with the others; rather
 * than block it, the card cascades to the nearest valid state, and each item
 * flags up-front which other axes it would change. Requests go through the real
 * proxy with the signed-in key, so a test is routed and metered like any SDK call
 * and shows up in the call log.
 */
export function Playground({ services, providers, catalog, defaultModel }: PlaygroundProps) {
  // The signed-in admin key doubles as a consumer key (the backend seeds an
  // admin user with the same key), so tests reuse it instead of a separate key.
  const apiKey = getAdminKey();

  // Per-model index: which testable wires it exposes and which providers serve
  // each. Every list the card offers and the cascade between them derive from it.
  const infos = useMemo(() => {
    const map = new Map<string, ModelInfo>();
    for (const s of services) {
      const serving = servingProviders(s, providers);
      const wires = wireTests(wiresOf(serving, wiresForModel(catalog, s.model))).map((t) => t.wire);
      const providersByWire = new Map<string, Provider[]>();
      for (const w of wires) {
        providersByWire.set(
          w,
          serving.filter((p) => p.endpoints.some((e) => e.wire === w)),
        );
      }
      map.set(s.model, { model: s.model, wires, providersByWire });
    }
    return map;
  }, [services, providers, catalog]);

  // The full universe of each axis, in a stable static order (independent of the
  // current selection) so an item never moves or disappears between renders.
  const allModels = useMemo(
    () => [...infos.keys()].sort((a, b) => modelMeta(a).name.localeCompare(modelMeta(b).name)),
    [infos],
  );
  const allTests = useMemo(() => {
    const ids = new Set<string>();
    for (const info of infos.values()) for (const w of info.wires) ids.add(w);
    return wireTests([...ids].sort());
  }, [infos]);
  const allProviders = useMemo(() => {
    const map = new Map<string, Provider>();
    for (const info of infos.values())
      for (const ps of info.providersByWire.values()) for (const p of ps) map.set(p.id, p);
    return [...map.values()].sort((a, b) => a.name.localeCompare(b.name));
  }, [infos]);

  // AUTO = let the gateway pick a provider; otherwise pin to a provider id. Each
  // axis holds a free choice from its full universe, so a pick may be incompatible
  // with the others — the pick* handlers below cascade to a valid state.
  const [sel, setSel] = useState<{ model: string; wire: string; providerId: string }>(() => ({
    model: defaultModel ?? '',
    wire: '',
    providerId: AUTO,
  }));

  // Normalize the raw selection against the current data so the display is always
  // valid (the seed may arrive late, or the model list may shift under us).
  const model = infos.has(sel.model)
    ? sel.model
    : defaultModel && infos.has(defaultModel)
      ? defaultModel
      : (allModels[0] ?? '');
  const info = infos.get(model);
  const wire = info && info.wires.includes(sel.wire) ? sel.wire : (info?.wires[0] ?? '');
  const routable = info?.providersByWire.get(wire) ?? [];
  const provider = routable.find((p) => p.id === sel.providerId);
  const test = allTests.find((t) => t.wire === wire);

  // Provider id for the snippets. The gateway routes every HTTP wire by endpoint
  // under Auto, so a snippet pins only when the user pinned a provider — except
  // WebSocket wires, which can't be Auto-routed and always need a concrete
  // provider (the pinned one, else the first serving the wire).
  const snippetProvider =
    provider ?? routable[0] ?? allProviders.find((p) => p.endpoints.some((e) => e.wire === wire));
  const snippetProviderId =
    test && wireNeedsProviderPin(test.wire) ? snippetProvider?.id : provider?.id;

  // --- cascade: honor the picked axis, minimally adjust the rest -------------
  const serves = (p: Provider, m: string, w: string) =>
    (infos.get(m)?.providersByWire.get(w) ?? []).some((x) => x.id === p.id);
  const servesAnyWire = (p: Provider, m: string) =>
    [...(infos.get(m)?.providersByWire.values() ?? [])].some((ps) => ps.some((x) => x.id === p.id));
  const keepProvider = (m: string, w: string) =>
    (infos.get(m)?.providersByWire.get(w) ?? []).some((p) => p.id === sel.providerId)
      ? sel.providerId
      : AUTO;

  const pickWire = (w: string) => {
    const m = info?.wires.includes(w) ? model : (allModels.find((x) => infos.get(x)?.wires.includes(w)) ?? model);
    setSel({ model: m, wire: w, providerId: keepProvider(m, w) });
  };
  const pickModel = (m: string) => {
    const w = infos.get(m)?.wires.includes(wire) ? wire : (infos.get(m)?.wires[0] ?? '');
    setSel({ model: m, wire: w, providerId: keepProvider(m, w) });
  };
  // Where selecting a routing provider lands: keep model/wire if it serves them,
  // else fall to the first model (then wire) it does serve.
  const routingResolve = (pid: string): { model: string; wire: string } => {
    if (pid === AUTO) return { model, wire };
    const p = allProviders.find((x) => x.id === pid);
    if (!p) return { model, wire };
    const m = servesAnyWire(p, model) ? model : (allModels.find((x) => servesAnyWire(p, x)) ?? model);
    const w =
      serves(p, m, wire) && m === model
        ? wire
        : (infos.get(m)?.wires.find((x) => serves(p, m, x)) ?? infos.get(m)?.wires[0] ?? '');
    return { model: m, wire: w };
  };
  const pickRouting = (pid: string) => setSel({ ...routingResolve(pid), providerId: pid });

  // --- per-item "Changes …" notes: what a pick would force on the OTHER axes -
  const note = (parts: string[]) => (parts.length ? `Changes ${parts.join(' + ')}` : undefined);
  // Endpoint and Wire are one 1:1 axis, so picking either swaps its sibling, and
  // may also switch the model when the current one doesn't serve the wire.
  const endpointNote = (w: string) =>
    w === wire ? undefined : note(['Wire', ...(info?.wires.includes(w) ? [] : ['Model'])]);
  const wireNote = (w: string) =>
    w === wire ? undefined : note(['Endpoint', ...(info?.wires.includes(w) ? [] : ['Model'])]);
  // Picking a model keeps it but may switch the endpoint+wire serving it.
  const modelNote = (m: string) => note(infos.get(m)?.wires.includes(wire) ? [] : ['Endpoint + Wire']);
  // Picking a provider may force a different model and/or endpoint+wire.
  const routingNote = (pid: string) => {
    if (pid === AUTO) return undefined;
    const r = routingResolve(pid);
    return note([...(r.wire !== wire ? ['Endpoint + Wire'] : []), ...(r.model !== model ? ['Model'] : [])]);
  };

  if (allModels.length === 0) return null;

  return (
    <div className={`card ${styles.section}`}>
      <div className={styles.head}>
        <h3 className={styles.title}>Test</h3>
      </div>

      <div className={styles.selectorRow}>
        <span className={styles.selectorLabel}>Endpoint</span>
        <Select value={wire} onValueChange={pickWire}>
          <SelectTrigger className={styles.selectorSelect} aria-label="Endpoint to test">
            <SelectValue placeholder="Select an endpoint" />
          </SelectTrigger>
          <SelectContent>
            {allTests.map((t) => (
              <SelectItem key={t.wire} value={t.wire} note={endpointNote(t.wire)}>
                {endpointPath(t)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className={styles.selectorRow}>
        <span className={styles.selectorLabel}>Wire</span>
        <Select value={wire} onValueChange={pickWire}>
          <SelectTrigger className={styles.selectorSelect} aria-label="Wire to test">
            <SelectValue placeholder="Select a wire" />
          </SelectTrigger>
          <SelectContent>
            {allTests.map((t) => (
              <SelectItem key={t.wire} value={t.wire} note={wireNote(t.wire)}>
                {wireFullName(t.wire)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className={styles.selectorRow}>
        <span className={styles.selectorLabel}>Model</span>
        <Select value={model} onValueChange={pickModel}>
          <SelectTrigger className={styles.selectorSelect} aria-label="Model to test">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {allModels.map((m) => (
              <SelectItem key={m} value={m} note={modelNote(m)}>
                {modelMeta(m).name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className={styles.selectorRow}>
        <span className={styles.selectorLabel}>Routing</span>
        <Select value={provider ? provider.id : AUTO} onValueChange={pickRouting}>
          <SelectTrigger className={styles.selectorSelect} aria-label="Routing">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={AUTO}>Auto</SelectItem>
            {allProviders.map((p) => (
              <SelectItem key={p.id} value={p.id} note={routingNote(p.id)}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {!test ? (
        <p className={styles.hint}>
          No provider serving this model exposes a model-serving wire to test interactively.
        </p>
      ) : (
        <>
          <Panel
            test={test}
            model={model}
            apiKey={apiKey}
            provider={provider}
            pinnedProviderId={snippetProviderId}
          />

          <div className={styles.codeIntro}>
            <span className={styles.codeIntroTitle}>Call it from your code</span>
            <span className={styles.codeIntroHint}>
              Filled with your signed-in key and routing — copy and run as-is.
            </span>
          </div>
          <CodeTabs
            key={`${model}:${test.wire}:${snippetProviderId ?? 'auto'}`}
            tabs={codeTabsFor(test.wire, {
              model,
              origin: window.location.origin,
              token: apiKey,
              providerId: snippetProviderId,
            })}
          />
        </>
      )}
    </div>
  );
}

/** Tabbed copy-runnable code samples (curl / Claude Code / Python) for a wire. */
function CodeTabs({ tabs }: { tabs: CodeTab[] }) {
  const [active, setActive] = useState(tabs[0]?.id);
  const current = tabs.find((t) => t.id === active) ?? tabs[0];
  if (!current) return null;
  return (
    <div className={styles.code}>
      <div className={styles.codeTabs}>
        {tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            className={`${styles.codeTab} ${t.id === current.id ? styles.codeTabActive : ''}`}
            onClick={() => setActive(t.id)}
          >
            {t.label}
          </button>
        ))}
        <CopyButton value={current.code} label="Copy" className={styles.codeCopy} />
      </div>
      <pre className={styles.raw}>{current.code}</pre>
    </div>
  );
}

/**
 * Dispatch to the panel for the active wire's kind. provider is the pinned
 * provider, or undefined under Auto routing — where the gateway routes by
 * endpoint, so ASR/TTS send no provider pin (empty id) just like chat.
 * pinnedProviderId is the resolved affinity provider for wires that can't be
 * Auto-routed (video): the pinned one, else the first serving the wire.
 */
function Panel({
  test,
  model,
  apiKey,
  provider,
  pinnedProviderId,
}: {
  test: WireTest;
  model: string;
  apiKey: string;
  provider?: Provider;
  pinnedProviderId?: string;
}) {
  switch (test.kind) {
    case 'chat':
    case 'embedding':
      return <PromptPanel test={test} model={model} apiKey={apiKey} providerId={provider?.id} />;
    case 'asr':
      return <AsrPanel apiKey={apiKey} providerId={provider?.id ?? ''} />;
    case 'tts':
      return <TtsPanel apiKey={apiKey} providerId={provider?.id ?? ''} model={model} />;
    case 'image':
      return <ImagePanel apiKey={apiKey} providerId={provider?.id} model={model} />;
    case 'video':
      return <VideoPanel apiKey={apiKey} providerId={pinnedProviderId ?? ''} model={model} />;
    default:
      return <UnsupportedPanel test={test} />;
  }
}

// --- Chat / embedding panel ------------------------------------------------

function PromptPanel({
  test,
  model,
  apiKey,
  providerId,
}: {
  test: WireTest;
  model: string;
  apiKey: string;
  providerId?: string;
}) {
  const isEmbedding = test.kind === 'embedding';
  const [prompt, setPrompt] = useState(
    isEmbedding
      ? 'The quick brown fox jumps over the lazy dog'
      : 'Hello! Reply in one short sentence.',
  );
  const [sending, setSending] = useState(false);
  const [result, setResult] = useState<TestResult | null>(null);
  const [showRaw, setShowRaw] = useState(false);

  const canSend = apiKey.trim() !== '' && prompt.trim() !== '' && !sending;

  const send = async () => {
    if (!canSend) return;
    setSending(true);
    setShowRaw(false);
    const req = buildTestRequest(model, test.wire, prompt);
    const res = await runTest(apiKey.trim(), req, providerId);
    setResult(res);
    setSending(false);
  };

  return (
    <>
      <textarea
        className={styles.prompt}
        rows={3}
        placeholder={isEmbedding ? 'Text to embed' : 'Message to send'}
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send();
          }
        }}
      />

      <div className={styles.actions}>
        <button type="button" className="btn btn-primary" onClick={send} disabled={!canSend}>
          {sending ? <span className="spinner" /> : <Send size={14} />}
          {sending ? 'Sending…' : 'Send'}
        </button>
        {result && !sending && <ResultMeta result={result} />}
      </div>

      {result && !sending && (
        <div className={styles.result}>
          {result.ok ? (
            <pre className={styles.resultText}>{result.text || '(empty response)'}</pre>
          ) : (
            <pre className={`${styles.resultText} ${styles.resultError}`}>{result.errorMessage}</pre>
          )}
          <RawToggle raw={result.raw} show={showRaw} onToggle={() => setShowRaw((v) => !v)} />
        </div>
      )}
    </>
  );
}

function ResultMeta({ result }: { result: TestResult }) {
  return (
    <span className={styles.meta}>
      <span className={`pill ${result.ok ? 'pill-ok' : 'pill-err'}`}>
        <span className="dot" />
        {result.status || 'network error'}
      </span>
      <span className={styles.metaItem}>{ms(result.latencyMs)}</span>
      {result.usage && (
        <span className={styles.metaItem}>
          {result.usage.input !== undefined && `${int(result.usage.input)} in`}
          {result.usage.input !== undefined && result.usage.output !== undefined && ' · '}
          {result.usage.output !== undefined && `${int(result.usage.output)} out`}
        </span>
      )}
    </span>
  );
}

// --- Image panel (OpenAI-compatible image generation) ----------------------

// Sizes valid for Doubao Seedream (≥ 3,686,400 px). DALL·E-scale 1024² is
// rejected by Seedream, so the defaults are 2K-class and up.
const IMAGE_SIZES = ['2048x2048', '2304x1728', '1728x2304', '2560x1440', '1440x2560'];

function ImagePanel({
  apiKey,
  providerId,
  model,
}: {
  apiKey: string;
  providerId?: string;
  model: string;
}) {
  const [prompt, setPrompt] = useState('A red panda coding on a laptop, warm studio lighting');
  const [size, setSize] = useState(IMAGE_SIZES[0]);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<ImageResult | null>(null);
  const [showRaw, setShowRaw] = useState(false);

  const canSend = apiKey.trim() !== '' && prompt.trim() !== '' && !running;

  const send = async () => {
    if (!canSend) return;
    setRunning(true);
    setShowRaw(false);
    const res = await runImage(apiKey.trim(), model, prompt.trim(), size, providerId);
    setResult(res);
    setRunning(false);
  };

  return (
    <>
      <p className={styles.hint}>
        Generate an image with the model’s OpenAI-compatible image endpoint. The result renders
        below — click an image to open it full size.
      </p>

      <div className={styles.fieldGrid}>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Size</span>
          <select className="select" value={size} onChange={(e) => setSize(e.target.value)}>
            {IMAGE_SIZES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
      </div>

      <textarea
        className={styles.prompt}
        rows={3}
        placeholder="Describe the image to generate"
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send();
          }
        }}
      />

      <div className={styles.actions}>
        <button type="button" className="btn btn-primary" onClick={send} disabled={!canSend}>
          {running ? <span className="spinner" /> : <Send size={14} />}
          {running ? 'Generating…' : 'Generate'}
        </button>
        {result && !running && (
          <span className={styles.meta}>
            <span className={`pill ${result.ok ? 'pill-ok' : 'pill-err'}`}>
              <span className="dot" />
              {result.status || 'network error'}
            </span>
            <span className={styles.metaItem}>{ms(result.latencyMs)}</span>
          </span>
        )}
      </div>

      {result && !running && (
        <div className={styles.result}>
          {result.ok ? (
            <div className={styles.imageGrid}>
              {result.images.map((src, i) => (
                <a key={i} href={src} target="_blank" rel="noreferrer">
                  <img className={styles.image} src={src} alt={`Generated result ${i + 1}`} />
                </a>
              ))}
            </div>
          ) : (
            <pre className={`${styles.resultText} ${styles.resultError}`}>{result.errorMessage}</pre>
          )}
          <RawToggle raw={result.raw} show={showRaw} onToggle={() => setShowRaw((v) => !v)} />
        </div>
      )}
    </>
  );
}

// --- Video panel (Volcengine Ark async task generation) --------------------

function VideoPanel({
  apiKey,
  providerId,
  model,
}: {
  apiKey: string;
  /** Resolved provider pin — video can't be Auto-routed; empty means none serve it. */
  providerId: string;
  model: string;
}) {
  const [prompt, setPrompt] = useState('A red panda surfing a wave at sunset, cinematic');
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<VideoResult | null>(null);
  const [showRaw, setShowRaw] = useState(false);

  const canSend = apiKey.trim() !== '' && prompt.trim() !== '' && providerId !== '' && !running;

  const send = async () => {
    if (!canSend) return;
    setRunning(true);
    setShowRaw(false);
    const res = await runVideo(apiKey.trim(), model, prompt.trim(), providerId);
    setResult(res);
    setRunning(false);
  };

  return (
    <>
      <p className={styles.hint}>
        Generate a video with the model’s Ark task API. This submits the job and polls until it’s
        ready (up to ~3 min), then plays the result below.
        {providerId === '' && ' Select a routing provider above first — video can’t be Auto-routed.'}
      </p>

      <textarea
        className={styles.prompt}
        rows={3}
        placeholder="Describe the video to generate"
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send();
          }
        }}
      />

      <div className={styles.actions}>
        <button type="button" className="btn btn-primary" onClick={send} disabled={!canSend}>
          {running ? <span className="spinner" /> : <Send size={14} />}
          {running ? 'Generating…' : 'Generate'}
        </button>
        {result && !running && (
          <span className={styles.meta}>
            <span className={`pill ${result.ok ? 'pill-ok' : 'pill-err'}`}>
              <span className="dot" />
              {result.status || 'network error'}
            </span>
            <span className={styles.metaItem}>{ms(result.latencyMs)}</span>
            {result.taskStatus && <span className={styles.metaItem}>{result.taskStatus}</span>}
          </span>
        )}
      </div>

      {result && !running && (
        <div className={styles.result}>
          {result.ok && result.videoUrl ? (
            <>
              <video className={styles.video} src={result.videoUrl} controls autoPlay loop />
              <a className={styles.download} href={result.videoUrl} target="_blank" rel="noreferrer">
                <Download size={13} /> Open video
              </a>
            </>
          ) : (
            <pre className={`${styles.resultText} ${styles.resultError}`}>{result.errorMessage}</pre>
          )}
          <RawToggle raw={result.raw} show={showRaw} onToggle={() => setShowRaw((v) => !v)} />
        </div>
      )}
    </>
  );
}

// --- ASR panel (Volcengine bigmodel file recognition) ----------------------

const ASR_FORMATS = ['wav', 'mp3', 'm4a', 'ogg', 'flac', 'pcm'];

function AsrPanel({
  apiKey,
  providerId,
}: {
  apiKey: string;
  providerId: string;
}) {
  const [audioUrl, setAudioUrl] = useState('');
  const [format, setFormat] = useState('wav');
  const [resourceId, setResourceId] = useState('volc.seedasr.auc');
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<AsrResult | null>(null);
  const [showRaw, setShowRaw] = useState(false);

  const canSend = apiKey.trim() !== '' && audioUrl.trim() !== '' && !running;

  const send = async () => {
    if (!canSend) return;
    setRunning(true);
    setShowRaw(false);
    const res = await runAsr({
      key: apiKey.trim(),
      providerId,
      resourceId: resourceId.trim() || 'volc.seedasr.auc',
      audioUrl: audioUrl.trim(),
      format,
    });
    setResult(res);
    setRunning(false);
  };

  return (
    <>
      <p className={styles.hint}>
        Transcribe a recording with ByteDance bigmodel file recognition. The audio must be at a
        publicly fetchable URL — the API pulls it by URL, then this polls until the transcript is
        ready.
      </p>

      <div className={styles.fieldGrid}>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Format</span>
          <select className="select" value={format} onChange={(e) => setFormat(e.target.value)}>
            {ASR_FORMATS.map((f) => (
              <option key={f} value={f}>
                {f}
              </option>
            ))}
          </select>
        </label>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Resource id</span>
          <input
            className="input mono"
            value={resourceId}
            onChange={(e) => setResourceId(e.target.value)}
            placeholder="volc.seedasr.auc"
          />
        </label>
      </div>

      <input
        className={`input mono ${styles.urlInput}`}
        value={audioUrl}
        placeholder="https://…/recording.wav"
        onChange={(e) => setAudioUrl(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send();
          }
        }}
      />

      <div className={styles.actions}>
        <button type="button" className="btn btn-primary" onClick={send} disabled={!canSend}>
          {running ? <span className="spinner" /> : <Send size={14} />}
          {running ? 'Transcribing…' : 'Transcribe'}
        </button>
        {result && !running && <AsrMeta result={result} />}
      </div>

      {result && !running && (
        <div className={styles.result}>
          {result.ok ? (
            <>
              <pre className={styles.resultText}>{result.text || '(empty transcript)'}</pre>
              {result.utterances && result.utterances.length > 0 && (
                <Utterances utterances={result.utterances} />
              )}
            </>
          ) : (
            <pre className={`${styles.resultText} ${styles.resultError}`}>{result.errorMessage}</pre>
          )}
          <RawToggle raw={result.raw} show={showRaw} onToggle={() => setShowRaw((v) => !v)} />
        </div>
      )}
    </>
  );
}

function AsrMeta({ result }: { result: AsrResult }) {
  return (
    <span className={styles.meta}>
      <span className={`pill ${result.ok ? 'pill-ok' : 'pill-err'}`}>
        <span className="dot" />
        {result.apiStatus || result.status || 'network error'}
      </span>
      <span className={styles.metaItem}>{ms(result.latencyMs)}</span>
      {result.durationMs !== undefined && (
        <span className={styles.metaItem}>{(result.durationMs / 1000).toFixed(1)}s audio</span>
      )}
    </span>
  );
}

function Utterances({ utterances }: { utterances: AsrResult['utterances'] }) {
  if (!utterances) return null;
  return (
    <div className={styles.utterances}>
      {utterances.map((u, i) => (
        <div key={i} className={styles.utterance}>
          {(u.start_time !== undefined || u.speaker) && (
            <span className={styles.utteranceMeta}>
              {u.speaker ? `${u.speaker} ` : ''}
              {u.start_time !== undefined ? `${(u.start_time / 1000).toFixed(1)}s` : ''}
            </span>
          )}
          <span>{u.text}</span>
        </div>
      ))}
    </div>
  );
}

// --- TTS panel (Volcengine HTTP unidirectional synthesis) ------------------

const TTS_FORMATS = ['mp3', 'wav', 'ogg_opus'];

function TtsPanel({
  apiKey,
  providerId,
  model,
}: {
  apiKey: string;
  providerId: string;
  /** Default X-Api-Resource-Id — the catalog selects the model by its id. */
  model: string;
}) {
  const [text, setText] = useState('你好，欢迎使用松果语音合成。');
  const [voice, setVoice] = useState(DEFAULT_TTS_VOICE);
  const [format, setFormat] = useState('mp3');
  const [resourceId, setResourceId] = useState(model);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<TtsResult | null>(null);
  const [showRaw, setShowRaw] = useState(false);

  // Free the previous clip's object URL when a new result replaces it (or on unmount).
  useEffect(() => {
    return () => {
      if (result?.audioUrl) URL.revokeObjectURL(result.audioUrl);
    };
  }, [result]);

  const canSend =
    apiKey.trim() !== '' && text.trim() !== '' && voice.trim() !== '' && !running;

  const send = async () => {
    if (!canSend) return;
    setRunning(true);
    setShowRaw(false);
    const res = await runTts({
      key: apiKey.trim(),
      providerId,
      resourceId: resourceId.trim() || model,
      text: text.trim(),
      voice: voice.trim(),
      format,
    });
    setResult(res);
    setRunning(false);
  };

  return (
    <>
      <p className={styles.hint}>
        Synthesize speech with Volcengine 大模型语音合成. The clip plays back below — the voice id
        must be one enabled on the routed account.
      </p>

      <div className={styles.fieldGrid}>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Voice</span>
          <input
            className="input mono"
            value={voice}
            onChange={(e) => setVoice(e.target.value)}
            placeholder="zh_female_vv_jupiter_bigtts"
          />
        </label>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Format</span>
          <select className="select" value={format} onChange={(e) => setFormat(e.target.value)}>
            {TTS_FORMATS.map((f) => (
              <option key={f} value={f}>
                {f}
              </option>
            ))}
          </select>
        </label>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>Resource id</span>
          <input
            className="input mono"
            value={resourceId}
            onChange={(e) => setResourceId(e.target.value)}
            placeholder={model}
          />
        </label>
      </div>

      <textarea
        className={styles.prompt}
        rows={3}
        placeholder="Text to speak"
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            send();
          }
        }}
      />

      <div className={styles.actions}>
        <button type="button" className="btn btn-primary" onClick={send} disabled={!canSend}>
          {running ? <span className="spinner" /> : <Send size={14} />}
          {running ? 'Synthesizing…' : 'Synthesize'}
        </button>
        {result && !running && <TtsMeta result={result} />}
      </div>

      {result && !running && (
        <div className={styles.result}>
          {result.ok ? (
            <>
              <audio className={styles.audio} src={result.audioUrl} controls autoPlay />
              <a className={styles.download} href={result.audioUrl} download={`speech.${result.ext}`}>
                <Download size={13} /> Download
              </a>
            </>
          ) : (
            <pre className={`${styles.resultText} ${styles.resultError}`}>{result.errorMessage}</pre>
          )}
          <RawToggle raw={result.raw} show={showRaw} onToggle={() => setShowRaw((v) => !v)} />
        </div>
      )}
    </>
  );
}

function TtsMeta({ result }: { result: TtsResult }) {
  return (
    <span className={styles.meta}>
      <span className={`pill ${result.ok ? 'pill-ok' : 'pill-err'}`}>
        <span className="dot" />
        {result.status || 'network error'}
      </span>
      <span className={styles.metaItem}>{ms(result.latencyMs)}</span>
      {result.chars !== undefined && (
        <span className={styles.metaItem}>{int(result.chars)} chars</span>
      )}
      {result.bytes !== undefined && (
        <span className={styles.metaItem}>{(result.bytes / 1024).toFixed(1)} KB</span>
      )}
    </span>
  );
}

// --- Fallback panel for wires without an interactive test ------------------

function UnsupportedPanel({ test }: { test: WireTest }) {
  // The endpoint label carries the transport: "WS …" wires are WebSocket
  // streaming, which the browser can't drive; the rest are HTTP. Either way the
  // copy-runnable samples below cover it.
  const isWs = test.endpoint.startsWith('WS ');

  return (
    <p className={styles.hint}>
      {isWs ? (
        <>
          The <strong>{test.label}</strong> wire (<code>{test.wire}</code>) is a WebSocket streaming
          API, so it can’t be exercised from the dashboard: a browser’s WebSocket can’t attach the{' '}
          <code>Authorization</code>, <code>X-Songguo-Provider</code>, and{' '}
          <code>X-Api-Resource-Id</code> headers the gateway requires, and the session speaks
          Volcengine’s binary frame protocol. Use a client that can set headers — see the samples
          below.
        </>
      ) : (
        <>
          Interactive testing for the <strong>{test.label}</strong> wire (<code>{test.wire}</code>)
          isn’t available in the dashboard yet — use the samples below to call it through the gateway.
        </>
      )}
    </p>
  );
}

// --- shared ----------------------------------------------------------------

function RawToggle({ raw, show, onToggle }: { raw: string; show: boolean; onToggle: () => void }) {
  if (raw === '') return null;
  return (
    <>
      <button type="button" className={styles.rawToggle} onClick={onToggle}>
        {show ? 'Hide raw response' : 'Show raw response'}
      </button>
      {show && <pre className={styles.raw}>{raw}</pre>}
    </>
  );
}
