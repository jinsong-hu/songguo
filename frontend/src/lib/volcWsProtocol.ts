// Volcengine (ByteDance) "大模型" streaming speech binary protocol, in the
// browser. The streaming ASR and TTS WebSocket wires don't speak JSON-over-text;
// every message is a binary frame: a 4-byte header, an optional sequence number,
// a 4-byte big-endian payload length, then a gzip-compressed JSON or raw-audio
// payload. This module is the single place that knows that layout — the per-wire
// transports build the JSON and feed it through encode*/decodeFrame.
//
// It also carries the browser↔gateway auth shim: a browser WebSocket can't set
// headers, so the gateway accepts the credentials as Sec-WebSocket-Protocol
// tokens (see proxy applyWSSubprotocolAuth). wsAuthProtocols base64url-encodes
// them into valid subprotocol tokens.

// --- frame constants -------------------------------------------------------

const PROTOCOL_VERSION = 0b0001;
const DEFAULT_HEADER_SIZE = 0b0001; // in 4-byte words → a 4-byte header

/** Message type (high nibble of header byte 1). */
export const MessageType = {
  FullClientRequest: 0b0001,
  AudioOnlyRequest: 0b0010,
  FullServerResponse: 0b1001,
  AudioOnlyResponse: 0b1011,
  ErrorResponse: 0b1111,
} as const;

/** Message-type-specific flags (low nibble of header byte 1). */
export const Flags = {
  NoSeq: 0b0000, // no sequence number in the body
  PosSeq: 0b0001, // positive sequence, more frames follow (seq in body)
  LastNoSeq: 0b0010, // final frame, no sequence number
  NegSeq: 0b0011, // final frame, with sequence number in the body
} as const;

const Serialization = { Raw: 0b0000, JSON: 0b0001 } as const;
const Compression = { None: 0b0000, Gzip: 0b0001 } as const;

// --- gzip (browser CompressionStream) --------------------------------------

async function streamThrough(data: Uint8Array, ts: GenericTransformStream): Promise<Uint8Array> {
  const blob = new Blob([data]);
  const stream = blob.stream().pipeThrough(ts);
  const buf = await new Response(stream).arrayBuffer();
  return new Uint8Array(buf);
}

export function gzip(data: Uint8Array): Promise<Uint8Array> {
  return streamThrough(data, new CompressionStream('gzip'));
}

export function gunzip(data: Uint8Array): Promise<Uint8Array> {
  return streamThrough(data, new DecompressionStream('gzip'));
}

// --- encode ----------------------------------------------------------------

function header(messageType: number, flags: number, serialization: number, compression: number): Uint8Array {
  return new Uint8Array([
    (PROTOCOL_VERSION << 4) | DEFAULT_HEADER_SIZE,
    (messageType << 4) | flags,
    (serialization << 4) | compression,
    0x00, // reserved
  ]);
}

function uint32be(n: number): Uint8Array {
  const b = new Uint8Array(4);
  new DataView(b.buffer).setUint32(0, n, false);
  return b;
}

function concat(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/**
 * Encode a JSON config frame (FULL_CLIENT_REQUEST): gzip the JSON, then
 * [header][payload-size][payload]. The opening request of every session.
 */
export async function encodeFullClientRequest(obj: unknown, flags: number = Flags.NoSeq): Promise<Uint8Array> {
  const payload = await gzip(new TextEncoder().encode(JSON.stringify(obj)));
  return concat([
    header(MessageType.FullClientRequest, flags, Serialization.JSON, Compression.Gzip),
    uint32be(payload.length),
    payload,
  ]);
}

/**
 * Encode an audio chunk frame (AUDIO_ONLY_REQUEST): gzip the raw audio bytes,
 * then [header][payload-size][payload]. `last` marks the final chunk so the
 * server knows the stream is complete.
 */
export async function encodeAudioOnlyRequest(audio: Uint8Array, last: boolean): Promise<Uint8Array> {
  const payload = await gzip(audio);
  return concat([
    header(MessageType.AudioOnlyRequest, last ? Flags.LastNoSeq : Flags.NoSeq, Serialization.Raw, Compression.Gzip),
    uint32be(payload.length),
    payload,
  ]);
}

// --- decode ----------------------------------------------------------------

export interface Frame {
  messageType: number;
  flags: number;
  /** Sequence number, when the frame carries one. */
  sequence?: number;
  /** Decompressed payload bytes (gzip undone when the frame was compressed). */
  payload: Uint8Array;
  /** Parsed JSON, when the payload was JSON-serialized. */
  json?: unknown;
  /** Error code, for an ERROR_RESPONSE frame. */
  errorCode?: number;
}

/**
 * Decode one server frame. Server frames are [header][optional seq][payload],
 * where an ERROR_RESPONSE leads with a 4-byte code and a length-prefixed
 * message, and other frames lead with a 4-byte payload length. The payload is
 * gunzipped when the header flags gzip, and JSON-parsed when JSON-serialized.
 */
export async function decodeFrame(data: Uint8Array): Promise<Frame> {
  const headerSize = (data[0] & 0x0f) * 4;
  const messageType = data[1] >> 4;
  const flags = data[1] & 0x0f;
  const serialization = data[2] >> 4;
  const compression = data[2] & 0x0f;

  let rest = data.subarray(headerSize);
  const view = new DataView(rest.buffer, rest.byteOffset, rest.byteLength);

  let sequence: number | undefined;
  let errorCode: number | undefined;
  let payload: Uint8Array;

  if (messageType === MessageType.ErrorResponse) {
    errorCode = view.getUint32(0, false);
    const size = view.getUint32(4, false);
    payload = rest.subarray(8, 8 + size);
  } else {
    // A frame with the sequence bit set (PosSeq/NegSeq) leads with an int32 seq.
    if ((flags & 0b0001) !== 0) {
      sequence = view.getInt32(0, false);
      rest = rest.subarray(4);
    }
    const sizeView = new DataView(rest.buffer, rest.byteOffset, rest.byteLength);
    const size = sizeView.getUint32(0, false);
    payload = rest.subarray(4, 4 + size);
  }

  if (compression === Compression.Gzip && payload.length > 0) {
    payload = await gunzip(payload);
  }

  const frame: Frame = { messageType, flags, sequence, payload, errorCode };
  if (serialization === Serialization.JSON && payload.length > 0) {
    try {
      frame.json = JSON.parse(new TextDecoder().decode(payload));
    } catch {
      /* leave json undefined; the raw payload is still available */
    }
  }
  return frame;
}

/** True once a frame marks the end of the server stream (final/last flag). */
export function isFinalFrame(frame: Frame): boolean {
  return frame.flags === Flags.LastNoSeq || frame.flags === Flags.NegSeq;
}

// --- browser↔gateway auth shim --------------------------------------------

function base64url(s: string): string {
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * The Sec-WebSocket-Protocol values that smuggle the gateway credentials past
 * the browser's header-less WebSocket API. The gateway lifts them back into the
 * Authorization / X-Songguo-Provider / X-Api-Resource-Id headers and strips them
 * before the upstream handshake. resourceId is omitted when empty.
 */
export function wsAuthProtocols(token: string, providerId: string, resourceId: string): string[] {
  const protocols = [`songguo.auth.${base64url(token)}`, `songguo.provider.${base64url(providerId)}`];
  if (resourceId) protocols.push(`songguo.resource.${base64url(resourceId)}`);
  return protocols;
}

/** The gateway-relative ws(s):// URL for a wire path, from the page origin. */
export function wsUrl(path: string): string {
  return window.location.origin.replace(/^http/, 'ws') + path;
}
