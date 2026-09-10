// Index the preset catalog by model ID so pages can enrich the auto-derived
// service list with specs (context window, modalities, pricing, kind).

import type { Catalog, CatalogModel, Cost, CostWindow } from '../api/types';
import { wireKind } from './wires';

export interface CatalogInfo extends CatalogModel {
  /** Model id (the key in the vendor's model map). */
  model: string;
  /** Coarse kind derived from the endpoint serving it, e.g. "chat" | "embedding". */
  kind: string;
}

export function indexCatalog(catalog: Catalog | null): Map<string, CatalogInfo> {
  const map = new Map<string, CatalogInfo>();
  if (!catalog) return map;
  for (const vendor of Object.values(catalog)) {
    for (const ep of vendor.endpoints) {
      const kind = wireKind(ep.wire);
      for (const id of ep.models ?? []) {
        const m = vendor.models[id];
        if (m && !map.has(id)) map.set(id, { ...m, model: id, kind });
      }
    }
  }
  return map;
}

/** Compact context-window label: 128000 → "128K", 1048576 → "1M". */
export function contextLabel(context?: number): string | null {
  if (!context) return null;
  if (context >= 1_000_000) {
    const m = context / 1_000_000;
    return `${Number.isInteger(m) ? m : m.toFixed(1)}M`;
  }
  return `${Math.round(context / 1000)}K`;
}

export const MODALITY_LABEL: Record<string, string> = {
  text: 'Text',
  image: 'Vision',
  audio: 'Audio',
  video: 'Video',
};

/**
 * What a cost is denominated in, for display. The token axes are per 1M tokens;
 * the media axes are per single unit, and a model only ever declares one of them
 * (models.dev supplies no media rates at all, so they come from the
 * hand-maintained half of the catalog).
 *
 * Returns the first axis found — a cost mixing token and media axes is priced
 * additively and has no single basis, so the token one is named as the headline.
 */
export function rateBasis(cost: Cost): string {
  if (cost.input || cost.output || cost.cache_read || cost.cache_write) return 'per 1M tokens';
  if (cost.character) return 'per character';
  if (cost.second) return 'per second';
  if (cost.image) return 'per image';
  if (cost.call) return 'per call';
  return '—';
}

/**
 * Describes a cost's context brackets for display, e.g. "2x above 272K".
 * Empty when the model has none, which is most of them.
 *
 * The multiple is computed off the dominant token side so the label answers the
 * question an operator actually has — how much worse a long request gets — and
 * not merely that a bracket exists.
 */
export function tierLabel(cost: Cost): string {
  const tiers = (cost.tiers ?? []).filter((t) => t.tier?.type === 'context');
  if (tiers.length === 0) return '';
  const base = Math.max(cost.input ?? 0, cost.output ?? 0);
  const parts = tiers
    .slice()
    .sort((a, b) => a.tier.size - b.tier.size)
    .map((t) => {
      const at = Math.max(t.input ?? cost.input ?? 0, t.output ?? cost.output ?? 0);
      const mult = base > 0 && at > 0 ? at / base : 0;
      const size = contextLabel(t.tier.size) ?? String(t.tier.size);
      return mult > 0 ? `${round(mult)}x above ${size}` : `raised above ${size}`;
    });
  return parts.join(', ');
}

/**
 * Describes a cost's peak-hour rates for display, e.g.
 * "2x · Mon–Fri 09:00–12:00, 14:00–18:00 UTC+08:00". Empty when the model has
 * none, which is nearly all of them.
 *
 * Like tierLabel, the multiple is computed rather than left implicit: an
 * operator comparing providers needs to know how much worse the peak is, not
 * merely that one exists. The hours are shown in the VENDOR's offset, matching
 * how the vendor publishes them and how catalog.json stores them — converting to
 * the viewer's local zone would make the label impossible to check against the
 * vendor's own pricing page, which is the thing an operator actually does.
 */
export function peakLabel(cost: Cost): string {
  const schedules = cost.schedules ?? [];
  if (schedules.length === 0) return '';
  const base = Math.max(cost.input ?? 0, cost.output ?? 0);

  const parts: string[] = [];
  for (const s of schedules) {
    const windows = (s.when ?? []).filter((w) => w.type === 'weekly');
    if (windows.length === 0) continue;
    const at = Math.max(s.input ?? cost.input ?? 0, s.output ?? cost.output ?? 0);
    const mult = base > 0 && at > 0 ? at / base : 0;
    const label = groupWindows(windows);
    parts.push(mult > 0 ? `${round(mult)}x · ${label}` : `raised ${label}`);
  }
  return parts.join('; ');
}

const DAY_LABEL: Record<string, string> = {
  mon: 'Mon',
  tue: 'Tue',
  wed: 'Wed',
  thu: 'Thu',
  fri: 'Fri',
  sat: 'Sat',
  sun: 'Sun',
};
const DAY_ORDER = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];

/**
 * Renders a schedule's windows as one phrase. Windows that share a day set —
 * which is every real case, since a vendor's peak hours apply on the same days —
 * collapse to "Mon–Fri 09:00–12:00, 14:00–18:00" rather than repeating the days
 * per window.
 */
function groupWindows(windows: CostWindow[]): string {
  const days = dayLabel(windows[0].days ?? []);
  const sameDays = windows.every((w) => dayLabel(w.days ?? []) === days);
  const offset = windows[0].offset || 'Z';
  const sameOffset = windows.every((w) => (w.offset || 'Z') === offset);
  const zone = offset === 'Z' ? 'UTC' : `UTC${offset}`;

  if (!sameDays || !sameOffset) {
    return windows
      .map((w) => `${dayLabel(w.days ?? [])} ${w.start}–${w.end} ${(w.offset || 'Z') === 'Z' ? 'UTC' : `UTC${w.offset}`}`)
      .join(', ');
  }
  const hours = windows.map((w) => `${w.start}–${w.end}`).join(', ');
  return `${days} ${hours} ${zone}`;
}

/** "mon".."fri" → "Mon–Fri"; a non-contiguous set stays a list. */
function dayLabel(days: string[]): string {
  const idx = days
    .map((d) => DAY_ORDER.indexOf(d))
    .filter((i) => i >= 0)
    .sort((a, b) => a - b);
  if (idx.length === 0) return '';
  if (idx.length === 1) return DAY_LABEL[DAY_ORDER[idx[0]]];
  const contiguous = idx.every((v, i) => i === 0 || v === idx[i - 1] + 1);
  if (contiguous) {
    return `${DAY_LABEL[DAY_ORDER[idx[0]]]}–${DAY_LABEL[DAY_ORDER[idx[idx.length - 1]]]}`;
  }
  return idx.map((i) => DAY_LABEL[DAY_ORDER[i]]).join(', ');
}

function round(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}
