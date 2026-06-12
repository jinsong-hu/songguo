// Build, send, and summarize interactive test calls for the service
// playground. Requests go through the real /v1 proxy with a consumer user
// key, so a test exercises auth, routing, failover, and metering exactly like
// SDK traffic — and shows up in the call log and usage stats.

const TEST_KEY_STORAGE = 'songguo_test_key';

export function getTestKey(): string {
  try {
    return localStorage.getItem(TEST_KEY_STORAGE) ?? '';
  } catch {
    return '';
  }
}

export function setTestKey(key: string): void {
  try {
    localStorage.setItem(TEST_KEY_STORAGE, key);
  } catch {
    /* ignore storage failures */
  }
}

export interface TestRequest {
  /** Proxy path, e.g. "/v1/chat/completions". */
  path: string;
  body: Record<string, unknown>;
}

/**
 * Pick the request shape a model's providers can actually serve: embeddings
 * by catalog kind, Anthropic-native Messages when that is the only enabled
 * chat wire, otherwise OpenAI-compatible chat completions.
 */
export function buildTestRequest(
  model: string,
  kind: string,
  wires: string[],
  prompt: string,
): TestRequest {
  if (kind === 'embedding') {
    return { path: '/v1/embeddings', body: { model, input: prompt } };
  }
  if (wires.includes('anthropic/messages') && !wires.includes('openai/chat')) {
    return {
      path: '/v1/messages',
      body: { model, max_tokens: 1024, messages: [{ role: 'user', content: prompt }] },
    };
  }
  return {
    path: '/v1/chat/completions',
    body: { model, messages: [{ role: 'user', content: prompt }] },
  };
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

/** Send a test request with the consumer user key and summarize the outcome. */
export async function runTest(key: string, req: TestRequest): Promise<TestResult> {
  const start = performance.now();
  let res: Response;
  try {
    res = await fetch(req.path, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${key}`,
      },
      body: JSON.stringify(req.body),
    });
  } catch (e) {
    return {
      ok: false,
      status: 0,
      latencyMs: Math.round(performance.now() - start),
      text: '',
      errorMessage: e instanceof Error ? e.message : 'Network error',
      raw: '',
    };
  }

  const latencyMs = Math.round(performance.now() - start);
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
 * Summarize a successful response across the supported wire shapes:
 * OpenAI chat (choices[].message.content), Anthropic Messages (content[]
 * text blocks), and embeddings (data[].embedding).
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
