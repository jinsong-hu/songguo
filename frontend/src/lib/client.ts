import { svg as claudeCodeSvg } from 'thesvg/claude-code';
import { svg as codexOpenAISvg } from 'thesvg/codex-openai';

/**
 * Display helpers for `client_name` — the normalized caller client songguo parses
 * from the User-Agent (`calls.ParseClientInfo`). Shared by the session detail
 * tile and the Overview page's Clients filter.
 *
 * The labels deliberately diverge from the icon package's own `title`: thesvg
 * calls the second one "Codex (OpenAI)", which is the vendor-qualified name and
 * reads as noise beside "Claude Code" in a filter list.
 */
export function clientLabel(name: string): string {
  if (name === 'claude-code') return 'Claude Code';
  if (name === 'codex-openai') return 'Codex';
  // An unrecognized name is shown as recorded rather than relabeled "Unknown" —
  // the value came from the ledger and we don't invent a better one for it.
  return name;
}

/**
 * Raw SVG markup for a client, or '' when we have no icon. The markup carries a
 * viewBox but no intrinsic size, so every call site sizes it in CSS.
 */
export function clientIconSvg(name: string): string {
  if (name === 'claude-code') return claudeCodeSvg;
  if (name === 'codex-openai') return codexOpenAISvg;
  return '';
}
