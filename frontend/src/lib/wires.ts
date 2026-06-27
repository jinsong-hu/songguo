// Wire metadata shared across the provider/catalog UI. A wire is the global
// protocol unit (path + auth + usage); these maps give it a friendly label and
// a coarse kind for display. Unknown wires fall back to the raw id / "chat".

// Friendly labels for wire ids ("openai/chat" → "Chat Completions").
const WIRE_NAMES: Record<string, string> = {
  'openai/chat': 'Chat Completions',
  'openai/completions': 'Completions',
  'openai/responses': 'Responses',
  'openai/embeddings': 'Embeddings',
  'openai/images': 'Image generation',
  'openai/models': 'Models',
  'anthropic/messages': 'Messages',
  'anthropic/models': 'Models',
  'ark/video': 'Video generation',
  'volc/tts-unidirectional': 'TTS (unidirectional)',
  'volc/tts-unidirectional-stream': 'TTS (unidirectional stream)',
  'volc/tts-bidirectional': 'TTS (bidirectional)',
  'volc/asr-file': 'ASR (file)',
  'volc/asr-stream-async': 'ASR (streaming)',
  'volc/asr-stream-nostream': 'ASR (streaming, final-only)',
  'volc/voice-clone': 'Voice cloning',
};

// The vendor that owns each wire family. The owner shows the bare wire name
// ("Chat Completions"); compatible vendors get the owner prefixed
// ("OpenAI - Chat Completions"). Keyed by the wire-id prefix.
const WIRE_OWNER: Record<string, { id: string; name: string }> = {
  openai: { id: 'openai', name: 'OpenAI' },
  anthropic: { id: 'anthropic', name: 'Anthropic' },
  ark: { id: 'volcengine', name: 'Volcengine' },
  volc: { id: 'volcengine', name: 'Volcengine' },
};

// wireName labels a wire ("openai/chat" → "Chat Completions"). Pass the vendor
// id to apply the owner prefix for compatible vendors — the wire's owner keeps
// the bare name, everyone else is prefixed ("OpenAI - Chat Completions").
export function wireName(wire: string, vendorId?: string): string {
  const base = WIRE_NAMES[wire] ?? wire;
  const owner = WIRE_OWNER[wire.split('/')[0]];
  if (!owner || vendorId === undefined) return base;
  const owned = vendorId === owner.id || vendorId.startsWith(`${owner.id}-`);
  return owned ? base : `${owner.name} - ${base}`;
}

// wireFullName always owner-qualifies a wire ("openai/chat" → "OpenAI Chat
// Completions"), unlike wireName which prefixes only compatible vendors. Used
// where the wire stands alone (e.g. the test card) and must read in full.
export function wireFullName(wire: string): string {
  const base = WIRE_NAMES[wire] ?? wire;
  const owner = WIRE_OWNER[wire.split('/')[0]];
  return owner ? `${owner.name} ${base}` : base;
}

// Coarse kind per wire, used to label models and pick the playground request
// shape. Management wires (model listings) report "" — they serve no models.
const WIRE_KIND: Record<string, string> = {
  'openai/chat': 'chat',
  'openai/completions': 'chat',
  'openai/responses': 'chat',
  'openai/embeddings': 'embedding',
  'openai/images': 'image',
  'openai/models': '',
  'anthropic/messages': 'chat',
  'anthropic/models': '',
  'ark/video': 'video',
  'volc/tts-unidirectional': 'tts',
  'volc/tts-unidirectional-stream': 'tts',
  'volc/tts-bidirectional': 'tts',
  'volc/asr-file': 'stt',
  'volc/asr-stream-async': 'stt',
  'volc/asr-stream-nostream': 'stt',
  'volc/voice-clone': 'tts',
};

export function wireKind(wire: string): string {
  return WIRE_KIND[wire] ?? 'chat';
}

/** A wire is a model-bearing capability (not a management/listing endpoint). */
export function wireServesModels(wire: string): boolean {
  return wireKind(wire) !== '';
}

// The auth scheme (adapter) a wire family expects. Derived from the wire id so
// the user never picks it: openai/* → Bearer, anthropic/* → x-api-key+version,
// volc/* → x-api-key.
export function wireAdapter(wire: string): string {
  if (wire.startsWith('anthropic/')) return 'anthropic-compatible';
  if (wire.startsWith('volc/')) return 'volc-speech';
  return 'openai-compatible';
}
