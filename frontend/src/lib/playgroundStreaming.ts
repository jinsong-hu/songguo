// Browser transport for the streaming-ASR test. The browser does NOT speak the
// vendor's binary WebSocket protocol — that lives server-side in the gateway's
// /api/test driver (see backend internal/proxy/wstest.go). Here the browser just
// opens a plain WebSocket to /api/test/<wire-path>, streams 16 kHz mono 16-bit
// PCM as binary messages, signals end-of-audio with a text "eof", and receives
// the vendor's decoded JSON results back as text. Mic capture and file decode
// both normalize to 16 kHz mono PCM first.

import type { AsrUtterance } from './playground';

const DEFAULT_ASR_RESOURCE = 'volc.seedasr.sauc.duration';
const TARGET_RATE = 16000;
const WS_TIMEOUT_MS = 60_000;
const FILE_CHUNK_BYTES = 3200; // 100 ms of 16 kHz mono 16-bit PCM

export interface AsrStreamResult {
  ok: boolean;
  text: string;
  utterances?: AsrUtterance[];
  errorMessage?: string;
  /** Pretty-printed server JSON results, for the raw view. */
  raw: string;
  latencyMs: number;
  /** Audio bytes streamed up. */
  bytesUp: number;
}

interface SessionOpts {
  key: string;
  providerId: string;
  /** Wire path, e.g. "/api/v3/sauc/bigmodel_async". */
  path: string;
  onPartial?: (text: string) => void;
}

interface AsrSession {
  pushAudio(pcm: Uint8Array): void;
  end(): void;
  done: Promise<AsrStreamResult>;
}

/** The dev/prod ws(s):// URL for the test driver path. */
function testWsUrl(path: string): string {
  const base = import.meta.env.DEV
    ? `ws://${window.location.hostname}:8080`
    : window.location.origin.replace(/^http/, 'ws');
  return `${base}/api/test${path}`;
}

/**
 * Open a streaming-ASR test session against /api/test. Audio is pushed as PCM
 * and flushed to the gateway as binary frames; end() sends the "eof" marker so
 * the gateway finalizes the transcript. Resolves when the gateway closes (or on
 * error/timeout) with whatever transcript arrived.
 */
function openAsrSession(o: SessionOpts): AsrSession {
  const start = performance.now();
  const elapsed = () => Math.round(performance.now() - start);
  const url =
    `${testWsUrl(o.path)}?key=${encodeURIComponent(o.key)}` +
    `&provider=${encodeURIComponent(o.providerId)}` +
    `&resource=${encodeURIComponent(DEFAULT_ASR_RESOURCE)}`;

  const queue: Uint8Array[] = [];
  let ended = false;
  let settled = false;
  let opened = false;
  let bytesUp = 0;
  const frames: unknown[] = [];
  let lastText = '';
  let utterances: AsrUtterance[] | undefined;

  let resolveDone!: (r: AsrStreamResult) => void;
  const done = new Promise<AsrStreamResult>((res) => (resolveDone = res));

  let ws: WebSocket;
  try {
    ws = new WebSocket(url);
  } catch (e) {
    resolveDone({
      ok: false,
      text: '',
      errorMessage: e instanceof Error ? e.message : 'Failed to open WebSocket',
      raw: '',
      latencyMs: elapsed(),
      bytesUp: 0,
    });
    return { pushAudio: () => {}, end: () => {}, done };
  }
  ws.binaryType = 'arraybuffer';

  const finish = (r: { ok: boolean; errorMessage?: string }) => {
    if (settled) return;
    settled = true;
    clearTimeout(timer);
    try {
      ws.close();
    } catch {
      /* already closing */
    }
    resolveDone({
      ok: r.ok,
      text: lastText,
      utterances,
      errorMessage: r.errorMessage,
      raw: frames.length ? JSON.stringify(frames, null, 2) : '',
      latencyMs: elapsed(),
      bytesUp,
    });
  };

  const timer = setTimeout(
    () => finish({ ok: lastText !== '', errorMessage: lastText ? undefined : 'Timed out waiting for the transcript (60s).' }),
    WS_TIMEOUT_MS,
  );

  // Drain pushed PCM as binary frames; once ended and drained, send the "eof"
  // marker and keep the socket open for the final transcript.
  const pump = async () => {
    while (!settled) {
      if (queue.length === 0) {
        if (ended) {
          if (ws.readyState === WebSocket.OPEN) ws.send('eof');
          return;
        }
        await sleep(20);
        continue;
      }
      const chunk = queue.shift()!;
      bytesUp += chunk.length;
      if (ws.readyState !== WebSocket.OPEN) return;
      ws.send(chunk);
      await sleep(0); // yield so onmessage can interleave
    }
  };

  ws.onopen = () => {
    opened = true;
    void pump();
  };

  ws.onmessage = (ev) => {
    if (typeof ev.data !== 'string') return; // gateway sends decoded JSON as text
    let json: unknown;
    try {
      json = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (json && typeof json === 'object' && 'error' in json) {
      finish({ ok: false, errorMessage: String((json as { error: unknown }).error) });
      return;
    }
    frames.push(json);
    const parsed = asrResultOf(json);
    if (parsed.text) {
      lastText = parsed.text;
      o.onPartial?.(lastText);
    }
    if (parsed.utterances) utterances = parsed.utterances;
  };

  ws.onerror = () => {};
  ws.onclose = (ev) => {
    if (settled) return;
    if (lastText) {
      finish({ ok: true });
      return;
    }
    finish({
      ok: false,
      errorMessage: `Connection closed before a transcript (code=${ev.code}${ev.reason ? ` ${ev.reason}` : ''} opened=${opened}).`,
    });
  };

  return {
    pushAudio: (pcm) => {
      if (!settled) queue.push(pcm);
    },
    end: () => {
      ended = true;
    },
    done,
  };
}

// --- file source -----------------------------------------------------------

export interface AsrStreamFileParams {
  key: string;
  providerId: string;
  path: string;
  file: File;
  onPartial?: (text: string) => void;
}

/** Decode an uploaded recording to 16 kHz mono PCM and stream it, lightly paced. */
export async function runAsrStreamFile(p: AsrStreamFileParams): Promise<AsrStreamResult> {
  let pcm: Uint8Array;
  try {
    pcm = await fileToPcm16k(p.file);
  } catch (e) {
    return {
      ok: false,
      text: '',
      errorMessage: e instanceof Error ? `Could not decode audio: ${e.message}` : 'Could not decode audio',
      raw: '',
      latencyMs: 0,
      bytesUp: 0,
    };
  }
  const session = openAsrSession({ key: p.key, providerId: p.providerId, path: p.path, onPartial: p.onPartial });
  for (let off = 0; off < pcm.length; off += FILE_CHUNK_BYTES) {
    session.pushAudio(pcm.subarray(off, off + FILE_CHUNK_BYTES));
    await sleep(40); // pace so the server sees a stream, not one burst
  }
  session.end();
  return session.done;
}

/** Decode any browser-supported audio file to 16 kHz mono 16-bit PCM bytes. */
async function fileToPcm16k(file: File): Promise<Uint8Array> {
  const buf = await file.arrayBuffer();
  const decodeCtx = new AudioContext();
  let decoded: AudioBuffer;
  try {
    decoded = await decodeCtx.decodeAudioData(buf);
  } finally {
    void decodeCtx.close();
  }
  const frames = Math.max(1, Math.ceil(decoded.duration * TARGET_RATE));
  const offline = new OfflineAudioContext(1, frames, TARGET_RATE);
  const src = offline.createBufferSource();
  src.buffer = decoded;
  src.connect(offline.destination);
  src.start();
  const rendered = await offline.startRendering();
  return floatToPCM16(rendered.getChannelData(0));
}

// --- microphone source -----------------------------------------------------

export interface AsrMicController {
  stop(): void;
  done: Promise<AsrStreamResult>;
}

/**
 * Capture the microphone as 16 kHz mono PCM and stream it live, so the transcript
 * appears as you speak. Returns a controller: stop() ends the utterance. Throws
 * if mic permission is denied before the session opens.
 */
export async function startAsrMicStream(p: {
  key: string;
  providerId: string;
  path: string;
  onPartial?: (text: string) => void;
}): Promise<AsrMicController> {
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
  });
  const ctx = new AudioContext({ sampleRate: TARGET_RATE });
  const source = ctx.createMediaStreamSource(stream);
  const node = ctx.createScriptProcessor(4096, 1, 1);
  const sink = ctx.createGain();
  sink.gain.value = 0; // route to destination silently so onaudioprocess fires

  const session = openAsrSession({ key: p.key, providerId: p.providerId, path: p.path, onPartial: p.onPartial });

  node.onaudioprocess = (e) => session.pushAudio(floatToPCM16(e.inputBuffer.getChannelData(0)));
  source.connect(node);
  node.connect(sink);
  sink.connect(ctx.destination);

  let stopped = false;
  const stop = () => {
    if (stopped) return;
    stopped = true;
    node.onaudioprocess = null;
    node.disconnect();
    source.disconnect();
    sink.disconnect();
    stream.getTracks().forEach((t) => t.stop());
    void ctx.close();
    session.end();
  };

  return { stop, done: session.done };
}

/** Convert Float32 [-1,1] samples to little-endian 16-bit PCM bytes. */
function floatToPCM16(input: Float32Array): Uint8Array {
  const out = new Uint8Array(input.length * 2);
  const view = new DataView(out.buffer);
  for (let i = 0; i < input.length; i++) {
    const s = Math.max(-1, Math.min(1, input[i]));
    view.setInt16(i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return out;
}

// --- response parsing ------------------------------------------------------

/** Pull text + utterances from a vendor response (result may nest under data). */
function asrResultOf(json: unknown): { text: string; utterances?: AsrUtterance[] } {
  if (typeof json !== 'object' || json === null) return { text: '' };
  const obj = json as Record<string, unknown>;
  const result = (obj.result ?? (obj.data as { result?: unknown } | undefined)?.result) as
    | Record<string, unknown>
    | undefined;
  const src = result ?? obj;
  const text = typeof src.text === 'string' ? src.text : '';
  const utterances = Array.isArray(src.utterances) ? (src.utterances as AsrUtterance[]) : undefined;
  return { text, utterances };
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
