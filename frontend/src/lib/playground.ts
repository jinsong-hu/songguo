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
export type TestKind =
  | 'chat'
  | 'embedding'
  | 'asr'
  | 'asrstream'
  | 'tts'
  | 'image'
  | 'video'
  | 'unsupported';

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
  'openai/images': 'image',
  'ark/video': 'video',
  'volc/asr-file': 'asr',
  'volc/asr-stream-async': 'asrstream',
  'volc/asr-stream-nostream': 'asrstream',
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
  'openai/images': 'POST /v1/images/generations',
  'ark/video': 'POST /api/v3/contents/generations/tasks',
  'volc/asr-file': 'POST /api/v3/auc/bigmodel/submit',
  'volc/asr-stream-async': 'WS /api/v3/sauc/bigmodel_async',
  'volc/asr-stream-nostream': 'WS /api/v3/sauc/bigmodel_nostream',
  'volc/tts-unidirectional': 'POST /api/v3/tts/unidirectional',
  'volc/tts-unidirectional-stream': 'WS /api/v3/tts/unidirectional-stream',
  'volc/tts-bidirectional': 'WS /api/v3/tts/bidirection',
  'volc/voice-clone': 'POST /api/v3/tts/voice_clone',
};

// Wires deliberately kept off the test card. Voice cloning trains a billed
// voice slot and has no safe, side-effect-free test shape, so it is omitted
// until it gets a real, opt-in flow rather than shown as a dead snippet.
const UNTESTED_WIRES = new Set<string>(['volc/voice-clone']);

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
    if (UNTESTED_WIRES.has(wire)) continue; // deliberately omitted (see above)
    tests.push({
      wire,
      label: wireName(wire),
      kind: TEST_KIND[wire] ?? 'unsupported',
      endpoint: TEST_ENDPOINT[wire] ?? '',
    });
  }
  // Group by modality so every wire family stays together (text first, then
  // the media families, with speech — TTS then ASR — last and adjacent). Within
  // a family, interactively-testable wires lead, then by id for stability. The
  // Endpoint and Wire selectors both render this list, so they share one order.
  return tests.sort((a, b) => {
    const ra = MODALITY_RANK[wireKind(a.wire)] ?? 99;
    const rb = MODALITY_RANK[wireKind(b.wire)] ?? 99;
    if (ra !== rb) return ra - rb;
    const ta = a.kind === 'unsupported' ? 1 : 0;
    const tb = b.kind === 'unsupported' ? 1 : 0;
    if (ta !== tb) return ta - tb;
    return a.wire.localeCompare(b.wire);
  });
}

// Display order of the wire families (from wireKind). Text wires lead; speech —
// TTS then ASR — comes last so the two stay grouped together.
const MODALITY_RANK: Record<string, number> = {
  chat: 0,
  embedding: 1,
  image: 2,
  video: 3,
  tts: 4,
  stt: 5,
};

// --- Snippet examples (single source for the "Try it" card and the playground
//     fallback) --------------------------------------------------------------

/** Default TTS voice — shared by the synthesis panel and its snippet. */
export const DEFAULT_TTS_VOICE = 'zh_female_vv_uranus_bigtts';

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
  if (endpoint.startsWith('WS ')) return wsSnippet(wire, endpoint.slice(3), opts);
  switch (wire) {
    case 'volc/asr-file':
      return asrFileSnippet(opts);
    case 'volc/tts-unidirectional':
      return ttsSnippet(opts);
    case 'openai/images':
      return imageSnippet(opts);
    case 'ark/video':
      return videoSnippet(opts);
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

/** The shared image-generation request body, so the snippets and the panel agree. */
const IMAGE_PROMPT = 'A red panda coding on a laptop, warm studio lighting';
const IMAGE_SIZE = '2048x2048';

function imageSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = [bearer(token), ...providerLines(providerId, false), `-H "Content-Type: application/json"`];
  const body = { model, prompt: IMAGE_PROMPT, size: IMAGE_SIZE };
  return `curl ${origin}/v1/images/generations \\
  ${headers.join(' \\\n  ')} \\
  -d '${JSON.stringify(body, null, 2)}'`;
}

/** The shared video-generation prompt, so the snippet and the panel agree. */
const VIDEO_PROMPT = 'A red panda surfing a wave at sunset, cinematic';

function videoSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  // Video is a submit→poll task API; the model isn't Auto-routable and the task
  // id is provider-specific, so both calls pin the same provider.
  const pin = `-H "X-Songguo-Provider: ${providerId || '<provider-id>'}"`;
  const submit = [bearer(token), pin, `-H "Content-Type: application/json"`];
  const poll = [bearer(token), pin];
  const body = { model, content: [{ type: 'text', text: VIDEO_PROMPT }] };
  return `# 1. Submit a generation task; the response carries a task "id".
TASK_ID=$(curl -s ${origin}/api/v3/contents/generations/tasks \\
  ${submit.join(' \\\n  ')} \\
  -d '${JSON.stringify(body, null, 2)}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# 2. Poll until status is "succeeded"; content.video_url holds the result.
curl ${origin}/api/v3/contents/generations/tasks/$TASK_ID \\
  ${poll.join(' \\\n  ')}`;
}

function asrFileSnippet({ origin, token, providerId }: SnippetOpts): string {
  // REQUEST_ID is generated once and reused across submit→poll, so the block
  // runs as-is; only the audio URL is genuine user input. Under Auto the gateway
  // routes by endpoint, so no provider header — pin one only to keep submit and
  // poll on the same provider when several serve this endpoint.
  const shared = [
    bearer(token),
    ...providerLines(providerId, false),
    `-H "X-Api-Resource-Id: volc.seedasr.auc"`,
    `-H "X-Api-Request-Id: $REQUEST_ID"`,
  ];
  const submit = [...shared, `-H "Content-Type: application/json"`];
  return `# 1. Submit a recording (audio fetched by URL).
REQUEST_ID=$(uuidgen)
curl ${origin}/api/v3/auc/bigmodel/submit \\
  ${submit.join(' \\\n  ')} \\
  -d '{ "user": {"uid":"me"}, "audio": {"url":"https://example.com/audio.wav","format":"wav"},
        "request": {"model_name":"bigmodel"} }'

# 2. Poll for the transcript with the same X-Api-Request-Id.
curl ${origin}/api/v3/auc/bigmodel/query \\
  ${shared.join(' \\\n  ')} -d '{}'`;
}

function wsSnippet(wire: string, path: string, { model, origin, token, providerId }: SnippetOpts): string {
  const wsOrigin = origin.replace(/^http/, 'ws');
  const headers = [
    bearer(token),
    `-H "X-Songguo-Provider: ${providerId || '<provider-id>'}"`,
    `-H "X-Api-Resource-Id: ${resourceIdFor(wire, model)}"`,
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

/** The X-Api-Resource-Id a Volcengine streaming wire bills under, for snippets. */
function resourceIdFor(wire: string, model: string): string {
  if (wire.startsWith('volc/tts')) return model; // TTS selects the model by its id
  if (wire.startsWith('volc/asr')) return 'volc.bigasr.sauc'; // streaming ASR billing class
  return '';
}

/**
 * Whether a wire still needs an explicit X-Songguo-Provider pin. The gateway
 * routes most HTTP wires by endpoint under Auto — model-routed and model-less
 * (ASR file, TTS) alike — so those never need a pin. Two cases still require
 * one: WebSocket wires (can't be Auto-routed), and ark/video, whose models the
 * vendor doesn't expose to Auto and whose submit→poll lifecycle must stay on one
 * provider (the task id is provider-specific).
 */
export function wireNeedsProviderPin(wire: string): boolean {
  if (wire === 'ark/video') return true;
  return (TEST_ENDPOINT[wire] ?? '').startsWith('WS ');
}

// --- Code-sample tabs (curl / Claude Code / Python) ------------------------

export interface CodeTab {
  id: 'curl' | 'claude-code' | 'python';
  label: string;
  /** Language hint (display only). */
  lang: string;
  code: string;
}

/**
 * The copy-runnable samples for a wire, in tab order: curl, Claude Code (only
 * for chat wires it can actually drive), then Python. Every gateway-known
 * placeholder — provider pin, bearer token, resource id — is filled from opts,
 * so the code runs as-is; only genuine user input (e.g. an audio URL) is left.
 */
export function codeTabsFor(wire: string, opts: SnippetOpts): CodeTab[] {
  const tabs: CodeTab[] = [
    { id: 'curl', label: 'curl', lang: 'bash', code: snippetFor(wire, opts) },
  ];
  if (wireKind(wire) === 'chat') {
    tabs.push({ id: 'claude-code', label: 'Claude Code', lang: 'bash', code: claudeCodeSnippet(opts) });
  }
  tabs.push({ id: 'python', label: 'Python', lang: 'python', code: pythonSnippetFor(wire, opts) });
  return tabs;
}

/** Point the Claude Code CLI at the gateway's Anthropic-compatible endpoint. */
function claudeCodeSnippet({ model, origin, token }: SnippetOpts): string {
  return `# Point Claude Code at this gateway, then start it.
export ANTHROPIC_BASE_URL="${origin}"
export ANTHROPIC_AUTH_TOKEN="${token || '<your-songguo-key>'}"
export ANTHROPIC_MODEL="${model}"
claude`;
}

// --- Python (requests) snippets — mirror the curl examples per wire ---------

/** The bearer value for a Python snippet; a clear placeholder when no key. */
function pyToken(token?: string): string {
  return token || '<your-songguo-key>';
}

/** Header dict lines (8-space indented) from key/value pairs. */
function pyHeaders(pairs: Array<[string, string]>): string {
  return pairs.map(([k, v]) => `        ${JSON.stringify(k)}: ${JSON.stringify(v)},`).join('\n');
}

/** Render a JSON-able value as a Python literal (True/False/None, dicts, lists). */
function pyValue(v: unknown, indent: number): string {
  const pad = ' '.repeat(indent);
  const pad2 = ' '.repeat(indent + 4);
  if (v === null) return 'None';
  if (typeof v === 'boolean') return v ? 'True' : 'False';
  if (typeof v === 'number') return String(v);
  if (typeof v === 'string') return JSON.stringify(v);
  if (Array.isArray(v)) {
    if (v.length === 0) return '[]';
    const items = v.map((x) => `${pad2}${pyValue(x, indent + 4)}`);
    return `[\n${items.join(',\n')},\n${pad}]`;
  }
  if (typeof v === 'object') {
    const entries = Object.entries(v as Record<string, unknown>);
    if (entries.length === 0) return '{}';
    const items = entries.map(([k, val]) => `${pad2}${JSON.stringify(k)}: ${pyValue(val, indent + 4)}`);
    return `{\n${items.join(',\n')},\n${pad}}`;
  }
  return 'None';
}

function pythonSnippetFor(wire: string, opts: SnippetOpts): string {
  const endpoint = TEST_ENDPOINT[wire] ?? '';
  if (endpoint.startsWith('WS ')) return pyWsSnippet(wire, endpoint.slice(3), opts);
  switch (wire) {
    case 'volc/asr-file':
      return pyAsrFileSnippet(opts);
    case 'volc/tts-unidirectional':
      return pyTtsSnippet(opts);
    case 'openai/images':
      return pyImageSnippet(opts);
    case 'ark/video':
      return pyVideoSnippet(opts);
    case 'openai/embeddings':
    case 'anthropic/messages':
    case 'openai/responses':
    case 'openai/completions':
    case 'openai/chat':
      return pyModelRoutedSnippet(wire, opts);
    default:
      return pyDefaultSnippet(opts);
  }
}

function pyProviderPair(providerId?: string): Array<[string, string]> {
  return providerId ? [['X-Songguo-Provider', providerId]] : [];
}

function pyModelRoutedSnippet(wire: string, { model, origin, token, providerId }: SnippetOpts): string {
  const prompt = wire === 'openai/embeddings' ? 'The quick brown fox' : 'Hello!';
  const req = buildTestRequest(model, wire, prompt);
  const headers = pyHeaders([
    ['Authorization', `Bearer ${pyToken(token)}`],
    ...pyProviderPair(providerId),
    ['Content-Type', 'application/json'],
  ]);
  return `import requests

resp = requests.post(
    "${origin}${req.path}",
    headers={
${headers}
    },
    json=${pyValue(req.body, 4)},
)
print(resp.json())`;
}

function pyTtsSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = pyHeaders([
    ['Authorization', `Bearer ${pyToken(token)}`],
    ...pyProviderPair(providerId),
    ['X-Api-Resource-Id', model],
    ['X-Control-Require-Usage-Tokens-Return', 'true'],
    ['Content-Type', 'application/json'],
  ]);
  const body = {
    user: { uid: 'me' },
    req_params: {
      text: '你好，世界',
      speaker: DEFAULT_TTS_VOICE,
      audio_params: { format: 'mp3', sample_rate: 24000 },
    },
  };
  return `import requests

resp = requests.post(
    "${origin}/api/v3/tts/unidirectional",
    headers={
${headers}
    },
    json=${pyValue(body, 4)},
)
# Newline-delimited JSON: each line carries a base64 audio chunk in "data".
print(resp.text)`;
}

function pyImageSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = pyHeaders([
    ['Authorization', `Bearer ${pyToken(token)}`],
    ...pyProviderPair(providerId),
    ['Content-Type', 'application/json'],
  ]);
  const body = { model, prompt: IMAGE_PROMPT, size: IMAGE_SIZE };
  return `import requests

resp = requests.post(
    "${origin}/v1/images/generations",
    headers={
${headers}
    },
    json=${pyValue(body, 4)},
)
# Each data[] item carries an image "url" or base64 "b64_json".
print(resp.json())`;
}

function pyVideoSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const pin = providerId || '<provider-id>';
  const body = { model, content: [{ type: 'text', text: VIDEO_PROMPT }] };
  return `import time
import requests

base = "${origin}/api/v3/contents/generations/tasks"
headers = {
    "Authorization": "Bearer ${pyToken(token)}",
    "X-Songguo-Provider": "${pin}",
    "Content-Type": "application/json",
}

# 1. Submit a generation task; the response carries a task "id".
task_id = requests.post(base, headers=headers, json=${pyValue(body, 4)}).json()["id"]

# 2. Poll until the task finishes; content.video_url holds the result.
while True:
    task = requests.get(f"{base}/{task_id}", headers=headers).json()
    if task.get("status") in ("succeeded", "failed", "cancelled"):
        print(task)
        break
    time.sleep(3)`;
}

function pyAsrFileSnippet({ origin, token, providerId }: SnippetOpts): string {
  const headerLines = [
    `    "Authorization": "Bearer ${pyToken(token)}",`,
    ...(providerId ? [`    "X-Songguo-Provider": "${providerId}",`] : []),
    `    "X-Api-Resource-Id": "volc.seedasr.auc",`,
    `    "X-Api-Request-Id": request_id,`,
    `    "Content-Type": "application/json",`,
  ];
  return `import time
import uuid
import requests

base = "${origin}/api/v3/auc/bigmodel"
request_id = str(uuid.uuid4())
headers = {
${headerLines.join('\n')}
}

# 1. Submit a recording (audio fetched by URL).
requests.post(f"{base}/submit", headers={**headers, "X-Api-Sequence": "-1"}, json={
    "user": {"uid": "me"},
    "audio": {"url": "https://example.com/audio.wav", "format": "wav"},
    "request": {"model_name": "bigmodel", "enable_itn": True, "enable_punc": True},
})

# 2. Poll for the transcript with the same X-Api-Request-Id.
while True:
    r = requests.post(f"{base}/query", headers=headers, json={})
    if r.headers.get("X-Api-Status-Code") in ("20000001", "20000002"):
        time.sleep(1.5)
        continue
    print(r.json())
    break`;
}

function pyWsSnippet(wire: string, path: string, { model, origin, token, providerId }: SnippetOpts): string {
  const wsOrigin = origin.replace(/^http/, 'ws');
  const pin = providerId || '<provider-id>';
  return `import asyncio
import websockets  # pip install websockets

async def main():
    async with websockets.connect(
        "${wsOrigin}${path}",
        additional_headers={
            "Authorization": "Bearer ${pyToken(token)}",
            "X-Songguo-Provider": "${pin}",
            "X-Api-Resource-Id": "${resourceIdFor(wire, model)}",
        },
    ) as ws:
        # Frames use Volcengine's binary, gzip-framed protocol — send/receive bytes here.
        ...

asyncio.run(main())`;
}

function pyDefaultSnippet({ model, origin, token, providerId }: SnippetOpts): string {
  const headers = pyHeaders([
    ['Authorization', `Bearer ${pyToken(token)}`],
    ...pyProviderPair(providerId),
    ['Content-Type', 'application/json'],
  ]);
  return `import requests

# ${model} is served over a model-less wire — call the native vendor path directly.
resp = requests.post(
    "${origin}/<vendor-path>",
    headers={
${headers}
    },
    json={"model": ${JSON.stringify(model)}, "...": "..."},
)
print(resp.json())`;
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

// --- Image transport (OpenAI-compatible image generation) ------------------

export interface ImageResult {
  ok: boolean;
  /** HTTP status; 0 for a network failure. */
  status: number;
  latencyMs: number;
  /** Renderable image sources (an http(s) url or a data: URI), one per image. */
  images: string[];
  errorMessage?: string;
  /** Pretty-printed response (base64 image data elided to stay readable). */
  raw: string;
}

/**
 * Generate an image via the OpenAI-compatible /v1/images/generations wire. The
 * call is model-routed (the model rides in the body), so under Auto it omits the
 * provider pin just like chat; pass providerId to pin one. The response carries
 * each image as a "url" or base64 "b64_json" — both are turned into a src the
 * panel can render.
 */
export async function runImage(
  key: string,
  model: string,
  prompt: string,
  size: string,
  providerId?: string,
): Promise<ImageResult> {
  const start = performance.now();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${key}`,
  };
  if (providerId) headers['X-Songguo-Provider'] = providerId;
  const body: Record<string, unknown> = { model, prompt };
  if (size) body.size = size;

  let res: Response;
  try {
    res = await fetch('/v1/images/generations', {
      method: 'POST',
      headers,
      body: JSON.stringify(body),
    });
  } catch (e) {
    return {
      ok: false,
      status: 0,
      latencyMs: elapsed(start),
      images: [],
      errorMessage: e instanceof Error ? e.message : 'Network error',
      raw: '',
    };
  }

  const latencyMs = elapsed(start);
  const bodyText = await res.text();
  const json = tryParse(bodyText);
  const raw = json !== null ? JSON.stringify(elideImageData(json), null, 2) : bodyText;

  if (!res.ok) {
    return {
      ok: false,
      status: res.status,
      latencyMs,
      images: [],
      errorMessage: errorMessageOf(json) ?? `Request failed (${res.status})`,
      raw,
    };
  }

  const images = imageSrcs(json);
  if (images.length === 0) {
    return { ok: false, status: res.status, latencyMs, images: [], errorMessage: 'No image returned.', raw };
  }
  return { ok: true, status: res.status, latencyMs, images, raw };
}

/** Pull renderable srcs from an images response: data[].url or data[].b64_json. */
function imageSrcs(json: unknown): string[] {
  if (typeof json !== 'object' || json === null) return [];
  const data = (json as { data?: unknown }).data;
  if (!Array.isArray(data)) return [];
  const out: string[] = [];
  for (const item of data) {
    if (typeof item !== 'object' || item === null) continue;
    const o = item as { url?: unknown; b64_json?: unknown };
    if (typeof o.url === 'string' && o.url !== '') out.push(o.url);
    else if (typeof o.b64_json === 'string' && o.b64_json !== '') out.push(`data:image/png;base64,${o.b64_json}`);
  }
  return out;
}

/** Replace long base64 image payloads with a short placeholder for the raw view. */
function elideImageData(json: unknown): unknown {
  if (typeof json !== 'object' || json === null) return json;
  const o = json as { data?: unknown };
  if (!Array.isArray(o.data)) return json;
  return {
    ...o,
    data: o.data.map((item) => {
      if (item && typeof item === 'object') {
        const b = (item as { b64_json?: unknown }).b64_json;
        if (typeof b === 'string' && b.length > 24) {
          return { ...(item as object), b64_json: `…${b.length} base64 chars` };
        }
      }
      return item;
    }),
  };
}

// --- Video transport (Volcengine Ark async task generation) ----------------

export interface VideoResult {
  ok: boolean;
  /** Last HTTP status from the gateway; 0 for a network failure. */
  status: number;
  /** Terminal task status, e.g. "succeeded" | "failed" | "cancelled". */
  taskStatus?: string;
  /** Result video URL, when the task succeeded. */
  videoUrl?: string;
  latencyMs: number;
  errorMessage?: string;
  /** Pretty-printed last response body. */
  raw: string;
}

/**
 * Run an Ark video-generation test: submit the task, then poll the task-status
 * GET (.../tasks/{id}) until it reaches a terminal state. Both calls pin the
 * same provider — the model isn't Auto-routable and the task id is
 * provider-specific, so submit and poll must stay on one upstream (the gateway
 * rewrites the unmatched poll path to the same base its submit went to). The
 * provider serving video must allow unmatched passthrough for the poll to reach
 * upstream.
 */
export async function runVideo(
  key: string,
  model: string,
  prompt: string,
  providerId: string,
): Promise<VideoResult> {
  const start = performance.now();
  const base = '/api/v3/contents/generations/tasks';
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${key}`,
  };
  if (providerId) headers['X-Songguo-Provider'] = providerId;

  // 1. Submit the task.
  let submit: Response;
  try {
    submit = await fetch(base, {
      method: 'POST',
      headers,
      body: JSON.stringify({ model, content: [{ type: 'text', text: prompt }] }),
    });
  } catch (e) {
    return videoNetworkError(start, e);
  }
  const submitText = await submit.text();
  const submitJson = tryParse(submitText);
  if (!submit.ok) {
    return {
      ok: false,
      status: submit.status,
      latencyMs: elapsed(start),
      errorMessage: errorMessageOf(submitJson) ?? `Submit failed (${submit.status})`,
      raw: pretty(submitJson, submitText),
    };
  }
  const taskId = videoTaskId(submitJson);
  if (!taskId) {
    return {
      ok: false,
      status: submit.status,
      latencyMs: elapsed(start),
      errorMessage: 'Submit returned no task id.',
      raw: pretty(submitJson, submitText),
    };
  }

  // 2. Poll until a terminal status or timeout (~3 min — video is slow).
  const deadline = performance.now() + 180_000;
  while (performance.now() < deadline) {
    await sleep(3000);
    let q: Response;
    try {
      q = await fetch(`${base}/${encodeURIComponent(taskId)}`, { method: 'GET', headers });
    } catch (e) {
      return videoNetworkError(start, e);
    }
    const text = await q.text();
    const json = tryParse(text);
    const raw = pretty(json, text);
    if (!q.ok) {
      return {
        ok: false,
        status: q.status,
        latencyMs: elapsed(start),
        errorMessage: errorMessageOf(json) ?? `Poll failed (${q.status})`,
        raw,
      };
    }
    const status = videoStatus(json);
    if (status === 'succeeded') {
      const videoUrl = videoUrlOf(json);
      return {
        ok: videoUrl !== '',
        status: q.status,
        taskStatus: status,
        videoUrl: videoUrl || undefined,
        latencyMs: elapsed(start),
        errorMessage: videoUrl ? undefined : 'Task succeeded but no video URL was returned.',
        raw,
      };
    }
    if (status === 'failed' || status === 'cancelled') {
      return {
        ok: false,
        status: q.status,
        taskStatus: status,
        latencyMs: elapsed(start),
        errorMessage: errorMessageOf(json) ?? `Task ${status}.`,
        raw,
      };
    }
    // queued / running / unknown → keep polling.
  }

  return {
    ok: false,
    status: 0,
    latencyMs: elapsed(start),
    errorMessage: 'Timed out waiting for the video (3 min).',
    raw: '',
  };
}

function videoNetworkError(start: number, e: unknown): VideoResult {
  return {
    ok: false,
    status: 0,
    latencyMs: elapsed(start),
    errorMessage: e instanceof Error ? e.message : 'Network error',
    raw: '',
  };
}

/** The task id from a submit response (top-level "id"). */
function videoTaskId(json: unknown): string {
  if (typeof json !== 'object' || json === null) return '';
  const id = (json as { id?: unknown }).id;
  return typeof id === 'string' ? id : '';
}

/** The lowercased task status from a poll response. */
function videoStatus(json: unknown): string {
  if (typeof json !== 'object' || json === null) return '';
  const s = (json as { status?: unknown }).status;
  return typeof s === 'string' ? s.toLowerCase() : '';
}

/** The result video URL: content.video_url, or any nested video_url field. */
function videoUrlOf(json: unknown): string {
  if (typeof json !== 'object' || json === null) return '';
  const content = (json as { content?: unknown }).content;
  if (typeof content === 'object' && content !== null) {
    const u = (content as { video_url?: unknown }).video_url;
    if (typeof u === 'string' && u !== '') return u;
  }
  const top = (json as { video_url?: unknown }).video_url;
  return typeof top === 'string' ? top : '';
}

// --- ASR transport (Volcengine bigmodel file recognition) ------------------

export interface AsrParams {
  /** Consumer user key (gateway auth). */
  key: string;
  /** Provider to pin via X-Songguo-Provider; empty routes by endpoint (Auto). */
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
    'X-Api-Resource-Id': p.resourceId,
    'X-Api-Request-Id': requestId,
  };
  // Under Auto (no pin) the gateway routes by endpoint; only pin when chosen.
  if (p.providerId) headers['X-Songguo-Provider'] = p.providerId;

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
  /** Provider to pin via X-Songguo-Provider; empty routes by endpoint (Auto). */
  providerId: string;
  /** Model/billing class header, e.g. "seed-tts-2.0". */
  resourceId: string;
  /** Text to synthesize. */
  text: string;
  /** Voice id (speaker), e.g. "zh_female_vv_uranus_bigtts". */
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
    'X-Api-Resource-Id': p.resourceId,
    'X-Api-Request-Id': uuid(),
    // Tell Volcengine to report usage so the call meters by character.
    'X-Control-Require-Usage-Tokens-Return': 'true',
  };
  // Under Auto (no pin) the gateway routes by endpoint; only pin when chosen.
  if (p.providerId) headers['X-Songguo-Provider'] = p.providerId;

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
