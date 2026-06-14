import { useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { Send } from 'lucide-react';
import type { Provider } from '../api/types';
import {
  buildTestRequest,
  getTestKey,
  runAsr,
  runTest,
  setTestKey,
  wireTests,
  type AsrResult,
  type TestResult,
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
 * model. Each selector collapses when there is a single option. Requests go
 * through the real proxy with a consumer user key, so a test is routed and
 * metered like any SDK call and shows up in the call log.
 */
export function Playground({ model, wires, providers }: PlaygroundProps) {
  const [key, setKey] = useState(getTestKey);

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

      {tests.length > 1 && (
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
      )}

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

      <KeyInput value={key} onChange={setKey} />

      <Panel test={test} model={model} apiKey={key} provider={provider} providers={servable} />
    </div>
  );
}

function KeyInput({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <>
      <p className={styles.hint}>
        Sends a real request through the gateway with a user key (created on the{' '}
        <Link to="/users">Users</Link> page), so it is routed and metered like any SDK call.
      </p>
      <div className={styles.keyRow}>
        <input
          className={`input ${styles.keyInput}`}
          type="password"
          placeholder="User key (sk-…)"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          autoComplete="off"
        />
      </div>
    </>
  );
}

/**
 * Dispatch to the panel for the active wire's kind. provider is the pinned
 * provider, or undefined under Auto routing (the gateway picks). Passthrough
 * wires (ASR) need a concrete provider, so under Auto they fall back to the
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
    default:
      return <UnsupportedPanel test={test} model={model} />;
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
    setTestKey(apiKey.trim());
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
    setTestKey(apiKey.trim());
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

// --- Fallback panel for wires without an interactive test ------------------

function UnsupportedPanel({ test, model }: { test: WireTest; model: string }) {
  const snippet = curlFor(test.wire, model);
  return (
    <div className={styles.unsupported}>
      <p className={styles.hint}>
        Interactive testing for the <strong>{test.label}</strong> wire (<code>{test.wire}</code>)
        isn’t available in the dashboard yet. Call it directly through the gateway:
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

function curlFor(wire: string, model: string): string {
  const origin = window.location.origin;
  if (wire === 'volc/tts') {
    return `curl ${origin}/api/v3/tts/unidirectional \\
  -H "Authorization: Bearer $SONGGUO_TOKEN" \\
  -H "X-Songguo-Provider: <provider-id>" \\
  -H "X-Api-Resource-Id: volc.service_type.10029" \\
  -H "Content-Type: application/json" \\
  -d '{ "req_params": { "text": "你好，世界" } }'`;
  }
  return `curl ${origin}/<vendor-path> \\
  -H "Authorization: Bearer $SONGGUO_TOKEN" \\
  -H "X-Songguo-Provider: <provider-id>" \\
  -H "Content-Type: application/json" \\
  -d '{ "model": "${model}", "…": "…" }'`;
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
