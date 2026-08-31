import { describe, expect, it } from 'vitest';
import { wireTests } from './playground';

const ids = (wires: string[], preferred?: Set<string>) =>
  wireTests(wires, preferred).map((t) => t.wire);

describe('wireTests', () => {
  it('keeps a configured wire the catalog does not list for the model', () => {
    // The regression: gpt-image-2 mapped onto a relay's chat wires. The catalog
    // only knows it under openai/images-generate, but the operator configured chat — and
    // songguo forwards what it was told to forward, so the panel must offer it.
    expect(ids(['openai/chat', 'openai/responses'], new Set(['openai/images-generate']))).toEqual([
      'openai/chat',
      'openai/responses',
    ]);
  });

  it('ranks catalog-preferred wires first, ahead of modality order', () => {
    // Chat outranks image by modality, so the preference must sort above it or
    // an image model would default to a chat box.
    expect(ids(['openai/chat', 'openai/images-generate'], new Set(['openai/images-generate']))[0]).toBe(
      'openai/images-generate',
    );
  });

  it('falls back to modality order when the catalog has no opinion', () => {
    expect(ids(['openai/images-generate', 'openai/chat'])).toEqual(['openai/chat', 'openai/images-generate']);
    expect(ids(['openai/images-generate', 'openai/chat'], new Set())).toEqual([
      'openai/chat',
      'openai/images-generate',
    ]);
  });

  it('still drops management and deliberately untested wires', () => {
    expect(ids(['openai/models', 'volc/voice-clone', 'openai/chat'])).toEqual(['openai/chat']);
  });
});
