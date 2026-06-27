import { useEffect, useMemo, useState } from 'react';
import { Download, Send } from 'lucide-react';
import type { Catalog, Provider, Service } from '../api/types';
import { getAdminKey } from '../api/client';
import { modelMeta } from '../lib/modelBrand';
import {
  buildTestRequest,
  codeTabsFor,
  DEFAULT_TTS_VOICE,
  runAsr,
  runTest,
  runTts,
  wireNeedsProviderPin,
  wireTests,
  type AsrResult,
  type CodeTab,
  type TestResult,
  type TtsResult,
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

/** The wires a single provider exposes that actually serve the model. */
function providerWires(p: Provider, modelWires: string[]): string[] {
  return p.endpoints.map((e) => e.wire).filter((w) => modelWires.includes(w));
}

/**
 * Interactive test card for any model the gateway serves. It is wire-first —
 * pick a model, the API (wire) it exposes, then routing (Auto or a pinned
 * provider) — and self-contained: a host page seeds the default model and the
 * card derives wires and routing from the services/providers/catalog it is
 * given, so the same impl backs every service page (and anywhere else a test
 * card belongs). All three selectors are always shown, even with a single
 * option, so the model, API, and routing in play are explicit. Requests go
 * through the real proxy with the signed-in key, so a test is routed and metered
 * like any SDK call and shows up in the call log.
 */
export function Playground({ services, providers, catalog, defaultModel }: PlaygroundProps) {
  // The signed-in admin key doubles as a consumer key (the backend seeds an
  // admin user with the same key), so tests reuse it instead of a separate key.
  const apiKey = getAdminKey();

  // Models offered, by friendly name. The host seeds one (the service in view);
  // the user can switch to exercise any other served model from the same card.
  const models = useMemo(
    () =>
      services
        .map((s) => s.model)
        .sort((a, b) => modelMeta(a).name.localeCompare(modelMeta(b).name)),
    [services],
  );
  const [model, setModel] = useState(defaultModel ?? models[0] ?? '');
  // Keep the selection valid if the seed is absent or the model list shifts.
  const selModel = models.includes(model) ? model : (models[0] ?? '');

  // Routing: AUTO = let the gateway pick a provider; otherwise pin to a provider
  // id. Auto is the default and lets a test exercise the real routing. (Radix
  // Select forbids an empty-string value, so Auto uses a sentinel that no
  // provider id equals, which `find` resolves to undefined — i.e. no pin.)
  const [providerId, setProviderId] = useState(AUTO);
  const [active, setActive] = useState(0);

  // Per selected model: the providers serving it and the wires it actually
  // exposes (catalog-narrowed), from which routing and API options derive.
  const serving = useMemo(
    () => servingProviders(services.find((s) => s.model === selModel), providers),
    [services, providers, selModel],
  );
  const wires = useMemo(
    () => wiresOf(serving, wiresForModel(catalog, selModel)),
    [serving, catalog, selModel],
  );

  // Only providers with at least one interactively-testable wire for this model.
  const servable = useMemo(
    () => serving.filter((p) => wireTests(providerWires(p, wires)).length > 0),
    [serving, wires],
  );
  const provider = servable.find((p) => p.id === providerId);

  // The APIs offered depend on routing: the full model union under Auto, or
  // only the pinned provider's wires.
  const tests = useMemo(
    () => wireTests(provider ? providerWires(provider, wires) : wires),
    [provider, wires],
  );
  const test = tests[Math.min(active, tests.length - 1)];

  // Provider id for the snippets. The gateway routes every HTTP wire by endpoint
  // under Auto, so a snippet pins only when the user pinned a provider — except
  // WebSocket wires, which can't be Auto-routed and always need a concrete
  // provider (the pinned one, else the first servable serving the wire).
  const snippetProvider =
    provider ?? servable.find((p) => p.endpoints.some((e) => e.wire === test?.wire));
  const snippetProviderId =
    test && wireNeedsProviderPin(test.wire) ? snippetProvider?.id : provider?.id;

  // Switching model invalidates the pinned provider and selected wire; reset both.
  const changeModel = (m: string) => {
    setModel(m);
    setProviderId(AUTO);
    setActive(0);
  };

  if (models.length === 0) return null;

  return (
    <div className={`card ${styles.section}`}>
      <div className={styles.head}>
        <h3 className={styles.title}>Test</h3>
      </div>

      <div className={styles.selectorRow}>
        <span className={styles.selectorLabel}>Model</span>
        <Select value={selModel} onValueChange={changeModel}>
          <SelectTrigger className={styles.selectorSelect} aria-label="Model to test">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {models.map((m) => (
              <SelectItem key={m} value={m}>
                {modelMeta(m).name}
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
          <div className={styles.selectorRow}>
            <span className={styles.selectorLabel}>API</span>
            <Select
              value={test.wire}
              onValueChange={(w) => setActive(tests.findIndex((t) => t.wire === w))}
            >
              <SelectTrigger className={styles.selectorSelect} aria-label="API to test">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {tests.map((t) => (
                  <SelectItem key={t.wire} value={t.wire}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className={styles.selectorRow}>
            <span className={styles.selectorLabel}>Routing</span>
            <Select
              value={provider ? provider.id : AUTO}
              onValueChange={(v) => {
                setProviderId(v);
                setActive(0);
              }}
            >
              <SelectTrigger className={styles.selectorSelect} aria-label="Routing">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={AUTO}>Auto</SelectItem>
                {servable.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <Panel test={test} model={selModel} apiKey={apiKey} provider={provider} />

          <div className={styles.codeIntro}>
            <span className={styles.codeIntroTitle}>Call it from your code</span>
            <span className={styles.codeIntroHint}>
              Filled with your signed-in key and routing — copy and run as-is.
            </span>
          </div>
          <CodeTabs
            key={`${selModel}:${test.wire}:${snippetProviderId ?? 'auto'}`}
            tabs={codeTabsFor(test.wire, {
              model: selModel,
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
 */
function Panel({
  test,
  model,
  apiKey,
  provider,
}: {
  test: WireTest;
  model: string;
  apiKey: string;
  provider?: Provider;
}) {
  switch (test.kind) {
    case 'chat':
    case 'embedding':
      return <PromptPanel test={test} model={model} apiKey={apiKey} providerId={provider?.id} />;
    case 'asr':
      return <AsrPanel apiKey={apiKey} providerId={provider?.id ?? ''} />;
    case 'tts':
      return <TtsPanel apiKey={apiKey} providerId={provider?.id ?? ''} model={model} />;
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
