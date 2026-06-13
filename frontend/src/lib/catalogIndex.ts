// Index the preset catalog by model ID so pages can enrich the auto-derived
// service list with specs (context window, modalities, pricing, kind).

import type { Catalog, CatalogModel } from '../api/types';
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
  for (const vendor of catalog.vendors) {
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
