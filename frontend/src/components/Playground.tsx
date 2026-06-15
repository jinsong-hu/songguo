import { useEffect, useMemo, useState } from 'react';
import { Download, Send } from 'lucide-react';
import type { Provider } from '../api/types';
import { getAdminKey } from '../api/client';
import {
  buildTestRequest,
  DEFAULT_TTS_VOICE,
  runAsr,
  runTest,
  runTts,
  snippetFor,
  wireTests,
  type AsrResult,
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
  model: string;
  /** Wires that serve this model (catalog-filtered union across providers). */
  wires: string[];
  /** Provider objects serving this model. */
  providers: Provider[];
}

/** Routing sentinel for "let the gateway pick" (Radix Select forbids ""). */
const AUTO = '__auto__';

/** The wires a single provider exposes that actually serve this model. */
function providerWires(p: Provider, modelWires: string[]): string[] {
  return p.endpoints.map((e) => e.wire).filter((w) => modelWires.includes(w));
}

/**
 * Interactive test UI for one service. The flow is provider-first, then wire:
 * a model may be served by several providers, so the test pins one provider
 * (X-Songguo-Provider) and offers only the wires that provider serves for this
 * model. Both selectors are always shown, even with a single option, so the
 * routing and API in play are explicit. Requests go through the real proxy with
 * the signed-in key, so a test is routed and metered like any SDK call and shows
 * up in the call log.
 */
export function Playground({ model, wires, providers }: PlaygroundProps) {
  // The signed-in admin key doubles as a consumer key (the backend seeds an
  // admin user with the same key), so tests reuse it instead of a separate key.
  const apiKey = getAdminKey();

  // Only providers with at least one interactively-testable wire for this model.
  const servable = useMemo(
    () => providers.filter((p) => wireTests(providerWires(p, wires)).length > 0),
    [providers, wires],
  );

  // Routing: AUTO = let the gateway pick a provider; otherwise pin to a provider
  // id. Auto is the default and lets a test exercise the real routing. (Radix
  // Select forbids an empty-string value, so Auto uses a sentinel that no
  // provider id equals, which `find` resolves to undefined — i.e. no pin.)
  const [providerId, setProviderId] = useState(AUTO);
  const provider = servable.find((p) => p.id === providerId);

  // The APIs offered depend on routing: the full model union under Auto, or
  // only the pinned provider's wires.
  const tests = useMemo(
    () => wireTests(provider ? providerWires(provider, wires) : wires),
    [provider, wires],
  );
  const [active, setActive] = useState(0);
  const test = tests[Math.min(active, tests.length - 1)];

  if (servable.length === 0 || !test) {
    return (
      <div className={`card ${styles.section}`}>
        <div className={styles.head}>
          <h3 className={styles.title}>Test</h3>
        </div>
        <p className={styles.hint}>
          No provider serving this model exposes a model-serving wire to test interactively.
        </p>
      </div>
    );
  }

  return (
    <div className={`card ${styles.section}`}>
      <div className={styles.head}>
        <h3 className={styles.title}>Test</h3>
      </div>

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
          value={providerId}
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

      <Panel test={test} model={model} apiKey={apiKey} provider={provider} providers={servable} />
    </div>
  );
}

/**
 * Dispatch to the panel for the active wire's kind. provider is the pinned
 * provider, or undefined under Auto routing (the gateway picks). Passthrough
 * wires (ASR, TTS) need a concrete provider, so under Auto they fall back to the
 * first servable provider that exposes the wire.
 */
function Panel({
  test,
  model,
  apiKey,
  provider,
  providers,
}: {
  test: WireTest;
  model: string;
  apiKey: string;
  provider?: Provider;
  providers: Provider[];
}) {
  switch (test.kind) {
    case 'chat':
    case 'embedding':
      return <PromptPanel test={test} model={model} apiKey={apiKey} providerId={provider?.id} />;
    case 'asr': {
      const p = provider ?? providers.find((pp) => pp.endpoints.some((e) => e.wire === test.wire));
      return <AsrPanel apiKey={apiKey} providerId={p?.id ?? ''} />;
    }
    case 'tts': {
      const p = provider ?? providers.find((pp) => pp.endpoints.some((e) => e.wire === test.wire));
      return <TtsPanel apiKey={apiKey} providerId={p?.id ?? ''} model={model} />;
    }
    default:
      return (
        <UnsupportedPanel test={test} model={model} apiKey={apiKey} providerId={provider?.id} />
      );
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

function UnsupportedPanel({
  test,
  model,
  apiKey,
  providerId,
}: {
  test: WireTest;
  model: string;
  apiKey: string;
  /** Pinned provider id under explicit routing; undefined under Auto. */
  providerId?: string;
}) {
  // The endpoint label carries the transport: "WS …" wires are WebSocket
  // streaming, which the browser can't drive (see below); the rest are HTTP.
  const isWs = test.endpoint.startsWith('WS ');
  const snippet = snippetFor(test.wire, {
    model,
    origin: window.location.origin,
    token: apiKey.trim(),
    providerId,
  });

  return (
    <div className={styles.unsupported}>
      <p className={styles.hint}>
        {isWs ? (
          <>
            The <strong>{test.label}</strong> wire (<code>{test.wire}</code>) is a WebSocket
            streaming API, so it can’t be exercised from the dashboard: a browser’s WebSocket can’t
            attach the <code>Authorization</code>, <code>X-Songguo-Provider</code>, and{' '}
            <code>X-Api-Resource-Id</code> headers the gateway requires, and the session speaks
            Volcengine’s binary frame protocol. Use a client that can set headers — curl can:
          </>
        ) : (
          <>
            Interactive testing for the <strong>{test.label}</strong> wire (<code>{test.wire}</code>)
            isn’t available in the dashboard yet. Call it directly through the gateway:
          </>
        )}
      </p>
      {snippet && (
        <div className={styles.snippetBlock}>
          <div className={styles.snippetHead}>
            <CopyButton value={snippet} label="Copy" />
          </div>
          <pre className={styles.raw}>{snippet}</pre>
        </div>
      )}
    </div>
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
