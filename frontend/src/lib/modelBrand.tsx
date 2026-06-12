// Model branding: maps raw model IDs ("claude-opus-4-20250514") to a
// human display name, a one-line tagline, and the creator's brand icon.
// Matching is by ID prefix so models added by custom providers still get
// sensible cards; unknown families fall back to a generic icon.

import { Sparkles } from 'lucide-react';
import type { CSSProperties } from 'react';

import claudeSvg from '@lobehub/icons-static-svg/icons/claude-color.svg?raw';
import deepseekSvg from '@lobehub/icons-static-svg/icons/deepseek-color.svg?raw';
import doubaoSvg from '@lobehub/icons-static-svg/icons/doubao-color.svg?raw';
import geminiSvg from '@lobehub/icons-static-svg/icons/gemini-color.svg?raw';
import grokSvg from '@lobehub/icons-static-svg/icons/grok.svg?raw';
import hunyuanSvg from '@lobehub/icons-static-svg/icons/hunyuan-color.svg?raw';
import kimiSvg from '@lobehub/icons-static-svg/icons/kimi-color.svg?raw';
import metaSvg from '@lobehub/icons-static-svg/icons/meta-color.svg?raw';
import minimaxSvg from '@lobehub/icons-static-svg/icons/minimax-color.svg?raw';
import mistralSvg from '@lobehub/icons-static-svg/icons/mistral-color.svg?raw';
import openaiSvg from '@lobehub/icons-static-svg/icons/openai.svg?raw';
import qwenSvg from '@lobehub/icons-static-svg/icons/qwen-color.svg?raw';
import wenxinSvg from '@lobehub/icons-static-svg/icons/wenxin-color.svg?raw';
import yiSvg from '@lobehub/icons-static-svg/icons/yi-color.svg?raw';
import zhipuSvg from '@lobehub/icons-static-svg/icons/zhipu-color.svg?raw';

export interface Brand {
  /** Model creator, e.g. "Anthropic" — used for accessibility labels only. */
  vendor: string;
  /** Raw inline SVG markup (sized 1em, so font-size controls it). */
  svg: string;
  /** Brand accent used for the subtle card tint. */
  color: string;
  match: RegExp;
}

const BRANDS: Brand[] = [
  { vendor: 'OpenAI', svg: openaiSvg, color: '#10a37f', match: /^(gpt|chatgpt|o[1345](-|$)|text-embedding|dall-e|whisper|tts)/ },
  { vendor: 'Anthropic', svg: claudeSvg, color: '#d97757', match: /^claude/ },
  { vendor: 'DeepSeek', svg: deepseekSvg, color: '#4d6bfe', match: /^deepseek/ },
  { vendor: 'Doubao', svg: doubaoSvg, color: '#336df4', match: /^doubao/ },
  { vendor: 'Qwen', svg: qwenSvg, color: '#615ced', match: /^(qwen|qwq|qvq)/ },
  { vendor: 'Google', svg: geminiSvg, color: '#1c7dff', match: /^(gemini|gemma)/ },
  { vendor: 'xAI', svg: grokSvg, color: '#71717a', match: /^grok/ },
  { vendor: 'Meta', svg: metaSvg, color: '#0668e1', match: /^(meta-)?llama/ },
  { vendor: 'Mistral', svg: mistralSvg, color: '#fa520f', match: /^(mistral|mixtral|ministral|codestral|magistral|pixtral|devstral)/ },
  { vendor: 'Moonshot', svg: kimiSvg, color: '#1ec9b0', match: /^(kimi|moonshot)/ },
  { vendor: 'Zhipu', svg: zhipuSvg, color: '#3859ff', match: /^(glm|chatglm|zhipu)/ },
  { vendor: 'MiniMax', svg: minimaxSvg, color: '#f23f5d', match: /^(minimax|abab)/ },
  { vendor: 'Tencent', svg: hunyuanSvg, color: '#0072f5', match: /^hunyuan/ },
  { vendor: 'Baidu', svg: wenxinSvg, color: '#2932e1', match: /^(ernie|wenxin)/ },
  { vendor: '01.AI', svg: yiSvg, color: '#00a05a', match: /^yi-/ },
];

export function brandOf(model: string): Brand | null {
  const id = model.toLowerCase();
  return BRANDS.find((b) => b.match.test(id)) ?? null;
}

// Resellers whose vendor label doesn't name the model creator they front for.
const VENDOR_ALIASES: Array<[RegExp, string]> = [
  [/volcengine|ark|方舟|火山/i, 'Doubao'],
  [/dashscope|bailian|百炼|alibaba|阿里/i, 'Qwen'],
  [/google/i, 'Google'],
];

/**
 * Brand for a configured provider: match its catalog vendor label first
 * ("OpenAI", "Volcengine 火山引擎 (Ark / 方舟)"), then fall back to the first
 * model whose family we recognize.
 */
export function providerBrand(vendor: string, models: string[]): Brand | null {
  if (vendor) {
    const label = vendor.toLowerCase();
    const direct = BRANDS.find((b) => label.includes(b.vendor.toLowerCase()));
    if (direct) return direct;
    const alias = VENDOR_ALIASES.find(([re]) => re.test(vendor));
    if (alias) return BRANDS.find((b) => b.vendor === alias[1]) ?? null;
  }
  for (const m of models) {
    const brand = brandOf(m);
    if (brand) return brand;
  }
  return null;
}

// Tokens that need casing other than simple capitalization.
const SPECIAL_CASE: Record<string, string> = {
  gpt: 'GPT',
  deepseek: 'DeepSeek',
  minimax: 'MiniMax',
  glm: 'GLM',
  chatglm: 'ChatGLM',
  qwq: 'QwQ',
  qvq: 'QvQ',
  xai: 'xAI',
  ai: 'AI',
};

// Connector words stay lowercase mid-name: "Kimi for Coding".
const LOWERCASE_WORDS = new Set(['for', 'and', 'with', 'of', 'the']);

function titleToken(t: string): string {
  const lower = t.toLowerCase();
  if (SPECIAL_CASE[lower]) return SPECIAL_CASE[lower];
  if (LOWERCASE_WORDS.has(lower)) return lower;
  if (/^v\d/.test(lower)) return lower.toUpperCase(); // v4 → V4
  if (/^\d+(\.\d+)?[kbm]$/.test(lower)) return lower.toUpperCase(); // 32k → 32K, 70b → 70B
  if (/^\d/.test(lower)) return lower; // bare versions stay as-is: 4o, 3.5
  return lower.charAt(0).toUpperCase() + lower.slice(1);
}

/**
 * Derive a marketing-style display name from a raw model ID, e.g.
 * "claude-3-5-haiku-20241022" → "Claude 3.5 Haiku", "gpt-4o-mini" → "GPT-4o Mini".
 */
export function modelDisplayName(model: string): string {
  // Strip release-date / -latest suffixes; they're noise in a display name.
  let id = model.replace(/-(20\d{6}|20\d{2}-\d{2}-\d{2}|latest)$/i, '');
  // Keep only the model part of org-prefixed IDs like "meta-llama/Llama-3-70b".
  const slash = id.lastIndexOf('/');
  if (slash >= 0) id = id.slice(slash + 1);

  let tokens = id.split('-').filter(Boolean);

  // Merge consecutive bare-number tokens into a dotted version: 3-5 → 3.5.
  const merged: string[] = [];
  for (const t of tokens) {
    const prev = merged[merged.length - 1];
    if (prev !== undefined && /^\d$/.test(prev) && /^\d$/.test(t)) {
      merged[merged.length - 1] = `${prev}.${t}`;
    } else {
      merged.push(t);
    }
  }
  tokens = merged;

  // GPT keeps its hyphenated version: GPT-4o Mini, GPT-3.5 Turbo — but only
  // when the next token is a version ("gpt-image-2" → "GPT Image 2").
  if (tokens[0]?.toLowerCase() === 'gpt' && /^\d/.test(tokens[1] ?? '')) {
    return [`GPT-${tokens[1]}`, ...tokens.slice(2).map(titleToken)].join(' ');
  }
  // OpenAI o-series stays lowercase: o3 Mini.
  if (/^o[1345]$/.test(tokens[0] ?? '')) {
    return [tokens[0], ...tokens.slice(1).map(titleToken)].join(' ');
  }
  return tokens.map(titleToken).join(' ');
}

// First matching pattern wins; specific families before generic tiers.
const TAGLINES: Array<[RegExp, string]> = [
  [/embedding/, 'Turns text into vectors for search, clustering, and retrieval.'],
  [/image|dall-e|flux/, 'Creates and edits images from natural-language prompts.'],
  [/^claude.*opus/, 'Anthropic’s most capable model, built for the hardest problems.'],
  [/^claude.*sonnet/, 'The best balance of intelligence, speed, and cost.'],
  [/^claude.*haiku/, 'Near-instant responses with real intelligence.'],
  [/^gpt-4o-mini/, 'Fast, affordable intelligence for everyday tasks.'],
  [/^gpt-4o/, 'Flagship multimodal intelligence — text and vision in one model.'],
  [/reasoner|thinking|^o[1345](-|$)/, 'Thinks before it answers — deep reasoning for hard problems.'],
  [/coder|coding|codex|codestral|devstral|code(-|$)/, 'Tuned for writing, reviewing, and refactoring code.'],
  // Tier keywords are anchored to token boundaries so brand names don't
  // false-match ("minimax" contains both "mini" and "max").
  [/(^|[-_.])(mini|lite|flash|haiku|turbo|tiny|nano|small|highspeed)(?![a-z])/, 'Light and fast — instant answers at minimal cost.'],
  [/(^|[-_.])(max|opus|ultra|large|pro)(?![a-z])/, 'Flagship-grade intelligence for demanding work.'],
  [/(^|[-_.])(plus|sonnet|standard|chat)(?![a-z])/, 'The balanced choice — strong quality at everyday speed.'],
];

export function modelTagline(model: string): string {
  const id = model.toLowerCase();
  for (const [re, line] of TAGLINES) {
    if (re.test(id)) return line;
  }
  return 'General-purpose intelligence, ready for anything.';
}

// Capability badges derived from the model ID alone, so cards stay
// informative even for models the preset catalog doesn't know about.
const BADGES: Array<[RegExp, string]> = [
  [/embedding/, 'Embeddings'],
  [/image|dall-e|flux/, 'Image generation'],
  [/reasoner|thinking|^o[1345](-|$)/, 'Reasoning'],
  [/coder|codex|coding|codestral|devstral/, 'Code'],
  [/(^|[-_.])(mini|lite|flash|haiku|turbo|tiny|nano|small|highspeed)(?![a-z])/, 'Fast'],
  [/(^|[-_.])(max|opus|ultra|large)(?![a-z])/, 'Flagship'],
];

export function modelBadges(model: string): string[] {
  const id = model.toLowerCase();
  const badges = BADGES.filter(([re]) => re.test(id))
    .map(([, label]) => label)
    .slice(0, 3);
  return badges.length > 0 ? badges : ['Chat'];
}

export interface ModelMeta {
  name: string;
  tagline: string;
  vendor: string | null;
  color: string;
}

export function modelMeta(model: string): ModelMeta {
  const brand = brandOf(model);
  return {
    name: modelDisplayName(model),
    tagline: modelTagline(model),
    vendor: brand?.vendor ?? null,
    color: brand?.color ?? '#3f8f5b',
  };
}

interface BrandIconProps {
  brand: Brand | null;
  /** Accessibility label used when there is no brand to name. */
  label: string;
  size?: number;
  className?: string;
}

/** A brand mark; falls back to a generic spark when the brand is unknown. */
export function BrandIcon({ brand, label, size = 20, className }: BrandIconProps) {
  const style: CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    width: size,
    height: size,
    fontSize: size,
    lineHeight: 1,
    flex: 'none',
  };
  if (!brand) {
    return (
      <span className={className} style={style} role="img" aria-label={label}>
        <Sparkles size={size} />
      </span>
    );
  }
  return (
    <span
      className={className}
      style={style}
      role="img"
      aria-label={brand.vendor}
      dangerouslySetInnerHTML={{ __html: brand.svg }}
    />
  );
}

interface ModelIconProps {
  model: string;
  size?: number;
  className?: string;
}

/** The creator's brand mark for a model; falls back to a generic spark. */
export function ModelIcon({ model, size = 20, className }: ModelIconProps) {
  return <BrandIcon brand={brandOf(model)} label={model} size={size} className={className} />;
}
