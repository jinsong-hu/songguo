// Browser transports for the WebSocket streaming speech wires. Each opens a real
// WS through the gateway (auth + provider pin smuggled as subprotocol tokens, see
// volcWsProtocol.wsAuthProtocols), speaks Volcengine's binary frame protocol, and
// summarizes the outcome — so a streaming test is routed and metered like SDK
// traffic, just over WS instead of HTTP.
//
// The session-level parameters (resource id, audio format, the request config)
// are vendor-specific and surfaced as editable panel fields, so a contract tweak
// is a UI edit, not a code change.

import {
  decodeFrame,
  encodeAudioOnlyRequest,
  encodeFullClientRequest,
  isFinalFrame,
  MessageType,
  wsAuthProtocols,
  wsUrl,
  type Frame,
} from './volcWsProtocol';
import type { AsrUtterance } from './playground';

const WS_TIMEOUT_MS = 60_000;

// --- streaming ASR (Volcengine sauc bigmodel) ------------------------------

export interface AsrStreamParams {
  /** Consumer user key (gateway auth). */
  key: string;
  /** Provider to pin — WS can't be Auto-routed. */
  providerId: string;
  /** Billing/model class header, e.g. "volc.seedasr.sauc.duration". */
  resourceId: string;
  /** Gateway WS path, e.g. "/api/v3/sauc/bigmodel_async". */
  path: string;
  /** The recording bytes to stream. */
  audio: Uint8Array;
  /** Audio container, e.g. "wav" | "mp3" | "pcm" | "ogg". */
  format: string;
  /** Sample rate in Hz, e.g. 16000. */
  rate: number;
  /** Recognition model name in the request config, e.g. "bigmodel". */
  modelName: string;
  /** Called with the latest transcript as partial results stream in. */
  onPartial?: (text: string) => void;
}

export interface AsrStreamResult {
  ok: boolean;
  text: string;
  utterances?: AsrUtterance[];
  errorMessage?: string;
  /** Pretty-printed server JSON frames, for the raw view. */
  raw: string;
  latencyMs: number;
  /** Audio bytes streamed up. */
  bytesUp: number;
}

/**
 * Run a streaming ASR test: open the WS, send the JSON config, stream the audio
 * in gzip'd chunks, and collect transcript frames until the server signals the
 * end. The config shape mirrors the Volcengine bigmodel streaming API; tweak the
 * panel fields if a model wants different parameters.
 */
export function runAsrStream(p: AsrStreamParams): Promise<AsrStreamResult> {
  const start = performance.now();
  const elapsed = () => Math.round(performance.now() - start);
  const config = {
    user: { uid: 'songguo-playground' },
    audio: { format: p.format, rate: p.rate, bits: 16, channel: 1 },
    request: {
      model_name: p.modelName,
      enable_punc: true,
      enable_itn: true,
      show_utterances: true,
    },
  };

  return new Promise<AsrStreamResult>((resolve) => {
    let settled = false;
    const frames: unknown[] = [];
    let lastText = '';
    let utterances: AsrUtterance[] | undefined;

    let ws: WebSocket;
    try {
      ws = new WebSocket(wsUrl(p.path), wsAuthProtocols(p.key, p.providerId, p.resourceId));
    } catch (e) {
      resolve(errResult(elapsed(), e instanceof Error ? e.message : 'Failed to open WebSocket'));
      return;
    }
    ws.binaryType = 'arraybuffer';

    const finish = (r: Partial<AsrStreamResult> & { ok: boolean; errorMessage?: string }) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        ws.close();
      } catch {
        /* already closing */
      }
      resolve({
        ok: r.ok,
        text: r.text ?? lastText,
        utterances: r.utterances ?? utterances,
        errorMessage: r.errorMessage,
        raw: frames.length ? JSON.stringify(frames, null, 2) : '',
        latencyMs: elapsed(),
        bytesUp: p.audio.length,
      });
    };

    const timer = setTimeout(
      () => finish({ ok: false, errorMessage: 'Timed out waiting for the transcript (60s).' }),
      WS_TIMEOUT_MS,
    );

    ws.onopen = async () => {
      try {
        ws.send(await encodeFullClientRequest(config));
        // Stream the audio in chunks, pacing slightly so the server sees a
        // genuine stream rather than one burst.
        const chunkSize = Math.max(p.rate, 8000); // ~1s of 16-bit mono audio
        for (let off = 0; off < p.audio.length; off += chunkSize) {
          if (settled) return;
          const slice = p.audio.subarray(off, off + chunkSize);
          const last = off + chunkSize >= p.audio.length;
          ws.send(await encodeAudioOnlyRequest(slice, last));
          if (!last) await sleep(40);
        }
      } catch (e) {
        finish({ ok: false, errorMessage: e instanceof Error ? e.message : 'Failed to send audio' });
      }
    };

    ws.onmessage = async (ev) => {
      let frame: Frame;
      try {
        frame = await decodeFrame(new Uint8Array(ev.data as ArrayBuffer));
      } catch (e) {
        finish({ ok: false, errorMessage: e instanceof Error ? e.message : 'Failed to decode frame' });
        return;
      }
      if (frame.json !== undefined) frames.push(frame.json);

      if (frame.messageType === MessageType.ErrorResponse) {
        finish({ ok: false, errorMessage: asrError(frame) });
        return;
      }
      const parsed = asrResultOf(frame.json);
      if (parsed.text) {
        lastText = parsed.text;
        p.onPartial?.(lastText);
      }
      if (parsed.utterances) utterances = parsed.utterances;

      if (isFinalFrame(frame)) finish({ ok: true });
    };

    ws.onerror = () => finish({ ok: false, errorMessage: 'WebSocket error (handshake or transport failed).' });
    ws.onclose = (ev) => {
      if (settled) return;
      // A clean close after we have a transcript is success; otherwise surface it.
      if (lastText) finish({ ok: true });
      else finish({ ok: false, errorMessage: `Connection closed (code ${ev.code}) before a transcript.` });
    };
  });
}

function errResult(latencyMs: number, errorMessage: string): AsrStreamResult {
  return { ok: false, text: '', errorMessage, raw: '', latencyMs, bytesUp: 0 };
}

/** Pull text + utterances from a bigmodel response frame (result may nest under data). */
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

/** The message from an ERROR_RESPONSE frame. */
function asrError(frame: Frame): string {
  if (frame.json && typeof frame.json === 'object') {
    const m = (frame.json as { message?: unknown; error?: unknown }).message;
    if (typeof m === 'string' && m) return m;
  }
  const text = new TextDecoder().decode(frame.payload).trim();
  if (text) return text;
  return frame.errorCode ? `Upstream error (code ${frame.errorCode})` : 'Upstream error';
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
