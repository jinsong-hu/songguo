// Derive a model's modality so the Services page can group the auto-derived
// service list. The reliable signal is which wires serve the model (per the
// catalog) → wireKind; off-catalog/custom models fall back to an id heuristic
// that mirrors the badge rules in modelBrand.

import type { Catalog } from '../api/types';
import { wireKind } from './wires';

// Coarse capability kinds, matching wireKind's vocabulary.
export type Kind = 'chat' | 'embedding' | 'image' | 'video' | 'tts' | 'stt';

// More specific kinds win when a model is served by several wires (a provider
// key may carry sibling wires); chat is the least specific.
const KIND_PRIORITY: Kind[] = ['embedding', 'image', 'video', 'tts', 'stt', 'chat'];

// Id-based fallback for models the catalog doesn't describe. First match wins.
const ID_KINDS: Array<[RegExp, Kind]> = [
  [/embedding/, 'embedding'],
  [/image|dall-e|flux|seedream|imagen/, 'image'],
  [/video|sora|seedance|veo|kling|wan-/, 'video'],
  [/whisper|transcrib|\basr\b|\bstt\b/, 'stt'],
  [/\btts\b|speech|voice|audio/, 'tts'],
];

function kindFromId(model: string): Kind {
  const id = model.toLowerCase();
  for (const [re, kind] of ID_KINDS) {
    if (re.test(id)) return kind;
  }
  return 'chat';
}

/** Wires that, per the catalog, serve this model (across all vendors). */
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

/** The coarse capability kind of a model: catalog wires first, then id heuristic. */
export function modelKind(model: string, catalog: Catalog | null): Kind {
  const kinds = new Set<string>();
  for (const wire of wiresForModel(catalog, model)) {
    const k = wireKind(wire);
    if (k) kinds.add(k);
  }
  for (const k of KIND_PRIORITY) {
    if (kinds.has(k)) return k;
  }
  return kindFromId(model);
}

export interface Bucket {
  /** Hugging Face pipeline-tag slug, e.g. "text-generation". */
  id: string;
  /** Hugging Face task name, e.g. "Text Generation". */
  label: string;
  kinds: Kind[];
}

// Capability buckets, keyed and named after Hugging Face's task taxonomy
// (https://huggingface.co/tasks). Buckets render in array order; empty ones are
// dropped, so the order just sets precedence among populated sections.
export const BUCKETS: Bucket[] = [
  { id: 'text-generation', label: 'Text Generation', kinds: ['chat'] },
  { id: 'feature-extraction', label: 'Feature Extraction', kinds: ['embedding'] },
  { id: 'text-to-image', label: 'Text-to-Image', kinds: ['image'] },
  { id: 'text-to-speech', label: 'Text-to-Speech', kinds: ['tts'] },
  { id: 'automatic-speech-recognition', label: 'Automatic Speech Recognition', kinds: ['stt'] },
  { id: 'text-to-video', label: 'Text-to-Video', kinds: ['video'] },
];
