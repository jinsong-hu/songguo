// Build, send, and summarize interactive test calls for the service
// playground. Requests go through the real /v1 (or /x for media wires) proxy
// with the signed-in key, so a test exercises auth, routing, failover, and
// metering exactly like SDK traffic — and shows up in the call log and usage
// stats.
//
// The panel is wire-driven, not chat-only: each model-serving wire maps to a
// test profile (which panel renders, which endpoint it hits). See wireTests.

import { wireKind, wireName } from './wires';

// --- Per-wire test profiles ------------------------------------------------

/** Which interactive panel a wire's test renders. */
export type TestKind = 'chat' | 'embedding' | 'asr' | 'tts' | 'unsupported';

export interface WireTest {
  /** Wire id, e.g. "openai/chat". */
  wire: string;
  /** Friendly wire label, e.g. "Chat Completions". */
  label: string;
  /** The panel to render for this wire. */
  kind: TestKind;
  /** Human-readable endpoint shown in the panel header. */
  endpoint: string;
}

// The panel kind per wire. Wires not listed but still model-serving fall back
// to an honest "not interactively testable" panel rather than a wrong chat box.
const TEST_KIND: Record<string, TestKind> = {
  'openai/chat': 'chat',
  'openai/completions': 'chat',
  'openai/responses': 'chat',
  'anthropic/messages': 'chat',
  'openai/embeddings': 'embedding',
  'volc/asr-file': 'asr',
  // Only the HTTP unidirectional TTS is fetch-drivable; the WebSocket stream and
  // bidirectional variants stay on the curl fallback.
  'volc/tts-unidirectional': 'tts',
};

// The endpoint label per wire. <provider> is a placeholder the media panels
// fill with the actual provider handle.
const TEST_ENDPOINT: Record<string, string> = {
  'openai/chat': 'POST /v1/chat/completions',
  'openai/completions': 'POST /v1/completions',
  'openai/responses': 'POST /v1/responses',
  'anthropic/messages': 'POST /v1/messages',
  'openai/embeddings': 'POST /v1/embeddings',
  'volc/asr-file': 'POST /api/v3/auc/bigmodel/submit',
  'volc/asr-stream-async': 'WS /api/v3/sauc/bigmodel_async',
  'volc/asr-stream-nostream': 'WS /api/v3/sauc/bigmodel_nostream',
  'volc/tts-unidirectional': 'POST /api/v3/tts/unidirectional',
  'volc/tts-unidirectional-stream': 'WS /api/v3/tts/unidirectional-stream',
  'volc/tts-bidirectional': 'WS /api/v3/tts/bidirection',
};

/**
 * Build the list of testable profiles for a model's enabled wires. Pure
 * management wires (model listings) are dropped — they serve no model, so
 * there is nothing to test. Everything else gets a panel; wires without a
 * dedicated one render the honest "unsupported" fallback.
 */
export function wireTests(wires: string[]): WireTest[] {
  const tests: WireTest[] = [];
  for (const wire of wires) {
    if (wireKind(wire) === '') continue; // model-listing / management wire
    tests.push({
      wire,
      label: wireName(wire),
      kind: TEST_KIND[wire] ?? 'unsupported',
      endpoint: TEST_ENDPOINT[wire] ?? '',
    });
  }
  // Stable, useful order: real interactive panels first, fallbacks last.
  const rank: Record<TestKind, number> = { chat: 0, embedding: 1, asr: 2, tts: 3, unsupported: 4 };
  return tests.sort((a, b) => rank[a.kind] - rank[b.kind]);
}

// --- Snippet examples (single source for the "Try it" card and the playground
//     fallback) --------------------------------------------------------------

/** Default TTS voice — shared by the synthesis panel and its snippet. */
export const DEFAULT_TTS_VOICE = 'zh_female_vv_jupiter_bigtts';

export interface SnippetOpts {
  /** Model id (model-routed bodies and the default placeholder). */
  model: string;
  /** Gateway origin, e.g. window.location.origin. */
  origin: string;
  /** Consumer key to fill the bearer token; falls back to $SONGGUO_TOKEN when absent. */
  token?: string;
  /** Pinned provider id; omitted under Auto for model-routed HTTP, shown as a
   *  placeholder for WebSocket and the submit→poll ASR-file wire. */
  providerId?: string;
}

/**
 * A copy-pasteable example for a wire. Both the service "Try it" card and the
 * playground's non-interactive fallback render this, so there is one source of
 * truth: the bearer token is filled from the signed-in key, and
 * X-Songguo-Provider follows the transport — model-routed HTTP omits it under
 * Auto, while WebSocket and the submit→poll ASR-file wire always show it (they
 * can't be model-routed / need provider affinity).
 */
export function snippetFor(wire: string, opts: SnippetOpts): string {
  const endpoint = TEST_ENDPOINT[wire] ?? '';
  if (endpoint.startsWith('WS ')) return wsSnippet(endpoint.slice(3), opts);
  switch (wire) {
    case 'volc/asr-file':
      return asrFileSnippet(opts);
    case 'volc/tts-unidirectional':
      return ttsSnippet(opts);
    case 'openai/embeddings':
    case 'anthropic/messages':
    case 'openai/responses':
    case 'openai/completions':
    case 'openai/chat':
      return modelRoutedSnippet(wire, opts);
    default:
      return defaultSnippet(opts);
  }
}

function bearer(token?: string): string {
  return `-H "Authorization: Bearer ${token || '$SONGGUO_TOKEN'}"`;
}

/** Provider header lines: pinned id, a placeholder when required, else nothing. */
function providerLines(providerId: string | undefined, required: boolean): string[] {
  if (providerId) return [`-H "X-Songguo-Provider: ${providerId}"`];
  if (required) return [`-H "X-Songguo-Provider: <provider-id>"`];
  return [];
}

function modelRoutedSnippet(wire: string, { model, origin, token, providerId }: SnippetOpts): string {
  const prompt = wire === 'openai/embeddings' ? 'The quick brown fox' : 'Hello!';
  const req = buildTestRequest(model, wire, prompt);
  const headers = [bearer(token), ...providerLines(providerId, false), `-H "Content-Type: application/json"`];
  return `curl ${origin}${req.path} \\
  ${headers.join(' \\\n  ')} \\
  -d '${JSON.stringify(req.body, null, 2)}'`;
}

function ttsSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = [
    bearer(token),
    ...providerLines(providerId, false),
    `-H "X-Api-Resource-Id: ${model}"`,
    `-H "X-Control-Require-Usage-Tokens-Return: true"`,
    `-H "Content-Type: application/json"`,
  ];
  const body = {
    user: { uid: 'me' },
    req_params: {
      text: '你好，世界',
      speaker: DEFAULT_TTS_VOICE,
      audio_params: { format: 'mp3', sample_rate: 24000 },
    },
  };
  return `curl ${origin}/api/v3/tts/unidirectional \\
  ${headers.join(' \\\n  ')} \\
  -d '${JSON.stringify(body, null, 2)}'`;
}

function asrFileSnippet({ origin, token, providerId }: SnippetOpts): string {
  const pin = providerId || '<provider-id>';
  const auth = bearer(token);
  return `# 1. Submit a recording (audio fetched by URL)
curl ${origin}/api/v3/auc/bigmodel/submit \\
  ${auth} \\
  -H "X-Songguo-Provider: ${pin}" \\
  -H "X-Api-Resource-Id: volc.seedasr.auc" \\
  -H "X-Api-Request-Id: $REQUEST_ID" \\
  -H "Content-Type: application/json" \\
  -d '{ "user": {"uid":"me"}, "audio": {"url":"https://…/audio.wav","format":"wav"},
        "request": {"model_name":"bigmodel"} }'

# 2. Poll for the transcript with the same provider pin and X-Api-Request-Id
curl ${origin}/api/v3/auc/bigmodel/query \\
  ${auth} \\
  -H "X-Songguo-Provider: ${pin}" \\
  -H "X-Api-Resource-Id: volc.seedasr.auc" \\
  -H "X-Api-Request-Id: $REQUEST_ID" -d '{}'`;
}

function wsSnippet(path: string, { origin, token, providerId }: SnippetOpts): string {
  const wsOrigin = origin.replace(/^http/, 'ws');
  const headers = [
    bearer(token),
    `-H "X-Songguo-Provider: ${providerId || '<provider-id>'}"`,
    `-H "X-Api-Resource-Id: <x-api-resource-id>"`,
  ];
  return `# WebSocket upgrade — curl (≥ 7.86) sets the auth headers a browser can't.
# This opens the authenticated stream; the audio/text frames that follow use
# Volcengine's binary, gzip-framed protocol, so drive them from a script.
curl --include --no-buffer \\
  ${headers.join(' \\\n  ')} \\
  "${wsOrigin}${path}"`;
}

function defaultSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = [bearer(token), ...providerLines(providerId, false), `-H "Content-Type: application/json"`];
  return `# ${model} is served over a model-less wire — call the native vendor path directly:
curl ${origin}/<vendor-path> \\
  ${headers.join(' \\\n  ')} \\
  -d '{ "model": "${model}", "…": "…" }'`;
}

// --- Chat / embedding transport (model-routed /v1) -------------------------

export interface TestRequest {
  /** Proxy path, e.g. "/v1/chat/completions". */
  path: string;
  body: Record<string, unknown>;
}

/** Pick the request shape for a chat/embedding wire. */
export function buildTestRequest(model: string, wire: string, prompt: string): TestRequest {
  switch (wire) {
    case 'openai/embeddings':
      return { path: '/v1/embeddings', body: { model, input: prompt } };
    case 'anthropic/messages':
      return {
        path: '/v1/messages',
        body: { model, max_tokens: 1024, messages: [{ role: 'user', content: prompt }] },
      };
    case 'openai/responses':
      return { path: '/v1/responses', body: { model, input: prompt } };
    case 'openai/completions':
      return { path: '/v1/completions', body: { model, prompt, max_tokens: 256 } };
    case 'openai/chat':
    default:
      return {
        path: '/v1/chat/completions',
        body: { model, messages: [{ role: 'user', content: prompt }] },
      };
  }
}

export interface TestUsage {
  input?: number;
  output?: number;
}

export interface TestResult {
  ok: boolean;
  /** HTTP status; 0 for a network failure. */
  status: number;
  latencyMs: number;
  /** Assistant text or embedding summary (empty on error). */
  text: string;
  /** Gateway/upstream error message when !ok. */
  errorMessage?: string;
  usage?: TestUsage;
  /** Pretty-printed response body for the raw view. */
  raw: string;
}

/**
 * Send a test request with the consumer user key and summarize the outcome.
 * When providerId is given, pin the request to that provider so a model served
 * by several providers is exercised one provider at a time (the gateway honors
 * X-Songguo-Provider on model-routed traffic).
 */
export async function runTest(
  key: string,
  req: TestRequest,
  providerId?: string,
): Promise<TestResult> {
  const start = performance.now();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${key}`,
  };
  if (providerId) headers['X-Songguo-Provider'] = providerId;
  let res: Response;
  try {
    res = await fetch(req.path, {
      method: 'POST',
      headers,
      body: JSON.stringify(req.body),
    });
  } catch (e) {
    return {
      ok: false,
      status: 0,
      latencyMs: elapsed(start),
      text: '',
      errorMessage: e instanceof Error ? e.message : 'Network error',
      raw: '',
    };
  }

  const latencyMs = elapsed(start);
  const bodyText = await res.text();
  let json: unknown = null;
  try {
    json = JSON.parse(bodyText);
  } catch {
    /* non-JSON body; raw text is still shown */
  }
  const raw = json !== null ? JSON.stringify(json, null, 2) : bodyText;

  if (!res.ok) {
    return {
      ok: false,
      status: res.status,
      latencyMs,
      text: '',
      errorMessage: errorMessageOf(json) ?? `Request failed (${res.status})`,
      raw,
    };
  }

  const { text, usage } = summarize(json);
  return { ok: true, status: res.status, latencyMs, text, usage, raw };
}

// --- ASR transport (Volcengine bigmodel file recognition) ------------------

export interface AsrParams {
  /** Consumer user key (gateway auth). */
  key: string;
  /** Provider id, pinned via X-Songguo-Provider (model-less, so no model to route on). */
  providerId: string;
  /** Billing class header, e.g. "volc.seedasr.auc". */
  resourceId: string;
  /** Publicly fetchable audio URL (the bigmodel file API pulls by URL). */
  audioUrl: string;
  /** Audio container, e.g. "wav" | "mp3" | "m4a". */
  format: string;
}

export interface AsrUtterance {
  text: string;
  start_time?: number;
  end_time?: number;
  speaker?: string;
}

export interface AsrResult {
  ok: boolean;
  /** Last HTTP status from the gateway. */
  status: number;
  /** ByteDance X-Api-Status-Code (e.g. "20000000" = done). */
  apiStatus?: string;
  text?: string;
  utterances?: AsrUtterance[];
  /** Recognized audio length, when reported. */
  durationMs?: number;
  latencyMs: number;
  errorMessage?: string;
  /** Pretty-printed last response body. */
  raw: string;
}

/**
 * Run a Volcengine bigmodel file-ASR test: submit the audio URL, then poll the
 * query endpoint (correlated by X-Api-Request-Id) until the transcript is
 * ready. Both calls hit the native /api/v3/... path and pin the same provider
 * via X-Songguo-Provider, so the gateway swaps in the upstream X-Api-Key, keeps
 * the submit→poll lifecycle on one provider, and meters the audio duration.
 */
export async function runAsr(p: AsrParams): Promise<AsrResult> {
  const start = performance.now();
  const requestId = uuid();
  const base = `/api/v3/auc/bigmodel`;
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${p.key}`,
    'X-Songguo-Provider': p.providerId,
    'X-Api-Resource-Id': p.resourceId,
    'X-Api-Request-Id': requestId,
  };

  // 1. Submit the recognition task.
  let submit: Response;
  try {
    submit = await fetch(`${base}/submit`, {
      method: 'POST',
      headers: { ...headers, 'X-Api-Sequence': '-1' },
      body: JSON.stringify({
        user: { uid: 'songguo-playground' },
        audio: { url: p.audioUrl, format: p.format },
        request: {
          model_name: 'bigmodel',
          enable_itn: true,
          enable_punc: true,
          show_utterances: true,
        },
      }),
    });
  } catch (e) {
    return asrNetworkError(start, e);
  }
  const submitText = await submit.text();
  if (!submit.ok) {
    const json = tryParse(submitText);
    return {
      ok: false,
      status: submit.status,
      apiStatus: submit.headers.get('X-Api-Status-Code') ?? undefined,
      latencyMs: elapsed(start),
      errorMessage: errorMessageOf(json) ?? `Submit failed (${submit.status})`,
      raw: pretty(json, submitText),
    };
  }

  // 2. Poll the query endpoint until done or timeout (~60s).
  const deadline = performance.now() + 60_000;
  while (performance.now() < deadline) {
    await sleep(1500);
    let q: Response;
    try {
      q = await fetch(`${base}/query`, { method: 'POST', headers, body: '{}' });
    } catch (e) {
      return asrNetworkError(start, e);
    }
    const text = await q.text();
    const json = tryParse(text);
    const raw = pretty(json, text);
    const apiStatus = q.headers.get('X-Api-Status-Code') ?? undefined;

    if (!q.ok) {
      return {
        ok: false,
        status: q.status,
        apiStatus,
        latencyMs: elapsed(start),
        errorMessage: errorMessageOf(json) ?? `Query failed (${q.status})`,
        raw,
      };
    }

    // Still running.
    if (apiStatus === '20000001' || apiStatus === '20000002') continue;

    const result = asrResultObject(json);
    const textOut = typeof result.text === 'string' ? result.text : '';
    const durationMs = asrDuration(json, result);

    // Silent audio: a successful but empty transcription.
    if (apiStatus === '20000003') {
      return {
        ok: true,
        status: q.status,
        apiStatus,
        text: textOut || '(silent audio — no speech detected)',
        durationMs,
        latencyMs: elapsed(start),
        raw,
      };
    }

    // Done (explicit success code, or a transcript appeared without a header).
    if (apiStatus === '20000000' || textOut) {
      return {
        ok: true,
        status: q.status,
        apiStatus,
        text: textOut,
        utterances: Array.isArray(result.utterances) ? (result.utterances as AsrUtterance[]) : undefined,
        durationMs,
        latencyMs: elapsed(start),
        raw,
      };
    }

    // A terminal but unrecognized status: stop rather than poll forever.
    if (apiStatus && apiStatus !== '20000000') {
      return {
        ok: false,
        status: q.status,
        apiStatus,
        latencyMs: elapsed(start),
        errorMessage: errorMessageOf(json) ?? `Unexpected status ${apiStatus}`,
        raw,
      };
    }
    // No status header and no result yet → keep polling.
  }

  return {
    ok: false,
    status: 0,
    latencyMs: elapsed(start),
    errorMessage: 'Timed out waiting for transcription (60s).',
    raw: '',
  };
}

// --- TTS transport (Volcengine HTTP unidirectional synthesis) --------------

export interface TtsParams {
  /** Consumer user key (gateway auth). */
  key: string;
  /** Provider id, pinned via X-Songguo-Provider (model-less, so no model to route on). */
  providerId: string;
  /** Model/billing class header, e.g. "seed-tts-2.0". */
  resourceId: string;
  /** Text to synthesize. */
  text: string;
  /** Voice id (speaker), e.g. "zh_female_vv_jupiter_bigtts". */
  voice: string;
  /** Audio container: "mp3" | "wav" | "ogg_opus". */
  format: string;
}

export interface TtsResult {
  ok: boolean;
  /** HTTP status from the gateway; 0 for a network failure. */
  status: number;
  latencyMs: number;
  /** Object URL for the synthesized audio (set on success). */
  audioUrl?: string;
  /** Audio MIME type, e.g. "audio/mpeg". */
  mime?: string;
  /** File extension for a download, e.g. "mp3". */
  ext?: string;
  /** Total decoded audio size in bytes. */
  bytes?: number;
  /** Characters billed (usage.text_words), when reported. */
  chars?: number;
  errorMessage?: string;
  /** Pretty-printed response (base64 audio elided to stay readable). */
  raw: string;
}

const TTS_MIME: Record<string, string> = {
  mp3: 'audio/mpeg',
  wav: 'audio/wav',
  ogg_opus: 'audio/ogg',
};

/**
 * Synthesize speech via the Volcengine HTTP unidirectional TTS endpoint. The
 * call pins a provider (X-Songguo-Provider — the wire is model-less) and asks
 * for usage so the gateway meters by character. The response is newline-
 * delimited JSON: each line carries a base64 audio chunk in "data", and a
 * trailing line reports usage.text_words. The chunks are concatenated into one
 * audio blob the panel can play back.
 */
export async function runTts(p: TtsParams): Promise<TtsResult> {
  const start = performance.now();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${p.key}`,
    'X-Songguo-Provider': p.providerId,
    'X-Api-Resource-Id': p.resourceId,
    'X-Api-Request-Id': uuid(),
    // Tell Volcengine to report usage so the call meters by character.
    'X-Control-Require-Usage-Tokens-Return': 'true',
  };

  let res: Response;
  try {
    res = await fetch('/api/v3/tts/unidirectional', {
      method: 'POST',
      headers,
      body: JSON.stringify({
        user: { uid: 'songguo-playground' },
        req_params: {
          text: p.text,
          speaker: p.voice,
          audio_params: { format: p.format, sample_rate: 24000 },
        },
      }),
    });
  } catch (e) {
    return {
      ok: false,
      status: 0,
      latencyMs: elapsed(start),
      errorMessage: e instanceof Error ? e.message : 'Network error',
      raw: '',
    };
  }

  const bodyText = await res.text();
  const latencyMs = elapsed(start);

  if (!res.ok) {
    const json = tryParse(bodyText);
    return {
      ok: false,
      status: res.status,
      latencyMs,
      errorMessage: errorMessageOf(json) ?? `Request failed (${res.status})`,
      raw: pretty(json, bodyText),
    };
  }

  // Success: parse the NDJSON stream, gathering audio chunks and usage.
  const chunks: Uint8Array[] = [];
  let chars: number | undefined;
  let apiError: string | undefined;
  const rawLines: unknown[] = [];

  for (const line of bodyText.split('\n')) {
    const trimmed = line.trim();
    if (trimmed === '') continue;
    const obj = tryParse(trimmed);
    if (obj === null || typeof obj !== 'object') {
      rawLines.push(trimmed);
      continue;
    }
    const o = obj as { code?: unknown; message?: unknown; data?: unknown; usage?: unknown };

    if (typeof o.data === 'string' && o.data !== '') {
      try {
        chunks.push(base64ToBytes(o.data));
      } catch {
        /* a malformed chunk is skipped; the raw view still shows the line */
      }
    }
    if (o.usage && typeof o.usage === 'object') {
      const w = (o.usage as Record<string, unknown>).text_words;
      if (typeof w === 'number') chars = w;
    }
    // An upstream mid-stream error rides a non-zero code on a line.
    if (typeof o.code === 'number' && o.code !== 0 && apiError === undefined) {
      apiError =
        typeof o.message === 'string' && o.message !== ''
          ? o.message
          : `Upstream error (code ${o.code})`;
    }
    rawLines.push(elideAudio(o));
  }

  const raw = JSON.stringify(rawLines, null, 2);

  if (chunks.length === 0) {
    return {
      ok: false,
      status: res.status,
      latencyMs,
      errorMessage: apiError ?? 'No audio returned.',
      raw,
    };
  }

  const bytes = chunks.reduce((n, c) => n + c.length, 0);
  const merged = new Uint8Array(bytes);
  let offset = 0;
  for (const c of chunks) {
    merged.set(c, offset);
    offset += c.length;
  }
  const mime = TTS_MIME[p.format] ?? 'audio/mpeg';
  const audioUrl = URL.createObjectURL(new Blob([merged], { type: mime }));

  return { ok: true, status: res.status, latencyMs, audioUrl, mime, ext: p.format, bytes, chars, raw };
}

/** Decode a standard base64 string to bytes. */
function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Replace a long base64 "data" field with a short placeholder for the raw view. */
function elideAudio(o: { data?: unknown }): unknown {
  if (typeof o.data === 'string' && o.data.length > 24) {
    return { ...o, data: `…${o.data.length} base64 chars` };
  }
  return o;
}

// --- shared helpers --------------------------------------------------------

function elapsed(start: number): number {
  return Math.round(performance.now() - start);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function uuid(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    const v = c === 'x' ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
}

function tryParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function pretty(json: unknown, fallback: string): string {
  return json !== null ? JSON.stringify(json, null, 2) : fallback;
}

function asrNetworkError(start: number, e: unknown): AsrResult {
  return {
    ok: false,
    status: 0,
    latencyMs: elapsed(start),
    errorMessage: e instanceof Error ? e.message : 'Network error',
    raw: '',
  };
}

/** Locate the ASR result object, which some payloads nest under "data". */
function asrResultObject(json: unknown): Record<string, unknown> {
  if (typeof json !== 'object' || json === null) return {};
  const obj = json as Record<string, unknown>;
  const fromData = (obj.data as { result?: unknown })?.result;
  const result = obj.result ?? fromData;
  return typeof result === 'object' && result !== null ? (result as Record<string, unknown>) : {};
}

/** Read audio_info.duration (ms) from either the top level or the result. */
function asrDuration(json: unknown, result: Record<string, unknown>): number | undefined {
  const fromTop = (json as { audio_info?: { duration?: unknown } })?.audio_info?.duration;
  const fromResult = (result as { audio_info?: { duration?: unknown } })?.audio_info?.duration;
  const d = typeof fromTop === 'number' ? fromTop : fromResult;
  return typeof d === 'number' ? d : undefined;
}

/**
 * Extract the error message from an error body. The gateway uses the OpenAI
 * envelope ({error: {message}}); responses forwarded verbatim from vendors
 * may use top-level "message" or "msg" instead.
 */
function errorMessageOf(json: unknown): string | null {
  if (typeof json !== 'object' || json === null) return null;
  const obj = json as { error?: { message?: unknown }; message?: unknown; msg?: unknown };
  for (const candidate of [obj.error?.message, obj.message, obj.msg]) {
    if (typeof candidate === 'string' && candidate !== '') return candidate;
  }
  return null;
}

/**
 * Summarize a successful response across the supported wire shapes: OpenAI
 * chat (choices[].message.content), OpenAI Responses (output[].content[] text),
 * Anthropic Messages (content[] text blocks), and embeddings (data[].embedding).
 */
function summarize(json: unknown): { text: string; usage?: TestUsage } {
  if (typeof json !== 'object' || json === null) return { text: '' };
  const obj = json as Record<string, unknown>;
  const usage = usageOf(obj);

  const choices = obj.choices;
  if (Array.isArray(choices) && choices.length > 0) {
    const first = choices[0] as { message?: { content?: unknown }; text?: unknown };
    const content = first.message?.content ?? first.text;
    return { text: typeof content === 'string' ? content : '', usage };
  }

  // OpenAI Responses API: output[].content[] with output_text blocks.
  const output = obj.output;
  if (Array.isArray(output)) {
    const text = output
      .flatMap((item) => {
        const content = (item as { content?: unknown }).content;
        return Array.isArray(content) ? content : [];
      })
      .filter((b): b is { text: string } =>
        typeof b === 'object' && b !== null && typeof (b as { text?: unknown }).text === 'string')
      .map((b) => b.text)
      .join('');
    if (text) return { text, usage };
  }

  const content = obj.content;
  if (Array.isArray(content)) {
    const text = content
      .filter((b): b is { type: string; text: string } =>
        typeof b === 'object' && b !== null &&
        (b as { type?: unknown }).type === 'text' &&
        typeof (b as { text?: unknown }).text === 'string')
      .map((b) => b.text)
      .join('');
    return { text, usage };
  }

  const data = obj.data;
  if (Array.isArray(data) && data.length > 0) {
    const embedding = (data[0] as { embedding?: unknown }).embedding;
    if (Array.isArray(embedding)) {
      const head = embedding
        .slice(0, 4)
        .map((v) => (typeof v === 'number' ? v.toFixed(5) : String(v)))
        .join(', ');
      return { text: `${embedding.length}-dimension vector  [${head}, …]`, usage };
    }
  }

  return { text: '', usage };
}

/** Read token usage from either the OpenAI or Anthropic usage shape. */
function usageOf(obj: Record<string, unknown>): TestUsage | undefined {
  const usage = obj.usage;
  if (typeof usage !== 'object' || usage === null) return undefined;
  const u = usage as Record<string, unknown>;
  const num = (v: unknown) => (typeof v === 'number' ? v : undefined);
  const input = num(u.prompt_tokens) ?? num(u.input_tokens);
  const output = num(u.completion_tokens) ?? num(u.output_tokens);
  if (input === undefined && output === undefined) return undefined;
  return { input, output };
}
