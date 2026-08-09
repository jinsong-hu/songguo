// Series colours for the dashboard charts. Extracted from Overview so every
// page that draws a breakdown gets the same key -> colour mapping; two pages
// colouring the same model differently would make them impossible to read
// against each other.

import type { UsageDimension } from '../api/types';
import { brandOf, providerBrand } from './modelBrand';

/** Convert a #rrggbb hex color to [hue, saturation, lightness] (h in 0..360, s/l in 0..1). */
function hexToHsl(hex: string): [number, number, number] {
  const m = hex.replace('#', '');
  const r = parseInt(m.slice(0, 2), 16) / 255;
  const g = parseInt(m.slice(2, 4), 16) / 255;
  const b = parseInt(m.slice(4, 6), 16) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (d === 0) return [0, 0, l];
  const s = d / (1 - Math.abs(2 * l - 1));
  let h: number;
  if (max === r) h = (((g - b) / d) % 6 + 6) % 6;
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  return [h * 60, s, l];
}

/**
 * Assigns a distinct color to every series key for the current Usage dimension.
 * Keys are grouped by their brand — the model creator's brand for the `model`
 * dimension, the provider's brand for `vendor` — then same-brand siblings are
 * spread across a wide lightness+hue ramp so they stay clearly distinguishable
 * in a stacked bar (e.g. Claude Opus vs Haiku, which used to collide). Anchoring
 * to the brand color keeps a series on-brand relative to other vendors. Keys we
 * can't brand (users, clients, unknown vendors) get evenly-spaced categorical
 * hues. "Other" is always the muted grey.
 *
 * A key's exact shade depends on which same-brand siblings are currently shown,
 * not on its name alone — the deliberate cost of guaranteeing sibling contrast.
 */
export function assignSeriesColors(
  keys: string[],
  dim: UsageDimension,
): Record<string, string> {
  const baseColor = (k: string): string | null => {
    if (dim === 'model') return brandOf(k)?.color ?? null;
    if (dim === 'vendor') return providerBrand(k, [])?.color ?? null;
    return null; // users and clients have no brand
  };

  // Partition into brand groups (keyed by base hex) plus one unbranded bucket.
  const branded = new Map<string, string[]>();
  const unbranded: string[] = [];
  for (const k of keys) {
    if (k === 'Other') continue;
    const base = baseColor(k);
    if (base) {
      const g = branded.get(base);
      if (g) g.push(k);
      else branded.set(base, [k]);
    } else {
      unbranded.push(k);
    }
  }

  const out: Record<string, string> = {};
  for (const [base, group] of branded) {
    const [h, s] = hexToHsl(base);
    const sat = Math.round(Math.min(0.85, Math.max(0.5, s)) * 100);
    const sorted = [...group].sort();
    const n = sorted.length;
    sorted.forEach((k, i) => {
      const t = n === 1 ? 0.5 : i / (n - 1); // 0..1 position within the group
      const light = Math.round(40 + t * 32); // 40%..72%
      const hue = (((h + (t - 0.5) * 34) % 360) + 360) % 360; // ±17° spread
      out[k] = `hsl(${hue} ${sat}% ${light}%)`;
    });
  }

  // Unbranded keys: evenly-spaced hues starting near the pine-green accent.
  const sortedU = [...unbranded].sort();
  const nu = sortedU.length;
  sortedU.forEach((k, i) => {
    const hue = Math.round(150 + (i / Math.max(1, nu)) * 300) % 360;
    out[k] = `hsl(${hue} 55% 55%)`;
  });

  out.Other = 'var(--text-muted)';
  return out;
}

/** Compact large numbers for axis ticks, e.g. 12.3k, 4.5M. */
export function compact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return `${Math.round(n)}`;
}
