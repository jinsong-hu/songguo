import { describe, expect, it } from 'vitest';
import { clientIconSvg, clientLabel } from './client';

describe('clientLabel', () => {
  it('names the clients we recognize', () => {
    expect(clientLabel('claude-code')).toBe('Claude Code');
    // Deliberately not thesvg's own title, which is "Codex (OpenAI)" — the
    // vendor qualifier is noise beside "Claude Code" in a filter list.
    expect(clientLabel('codex-openai')).toBe('Codex');
  });

  it('passes an unrecognized name through as recorded', () => {
    // Not "Unknown": the value came from the ledger and we do not invent a
    // better one for it.
    expect(clientLabel('some-new-agent')).toBe('some-new-agent');
  });
});

describe('clientIconSvg', () => {
  it('returns markup for the clients we recognize', () => {
    expect(clientIconSvg('claude-code')).toContain('<svg');
    expect(clientIconSvg('codex-openai')).toContain('<svg');
  });

  it('returns empty for anything else, so the caller can skip the icon slot', () => {
    expect(clientIconSvg('some-new-agent')).toBe('');
    expect(clientIconSvg('')).toBe('');
  });
});
