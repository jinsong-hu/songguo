// Index the preset catalog by model ID so pages can enrich the auto-derived
// service list with specs (context window, modalities, pricing, kind).

import type { Catalog, CatalogModel } from '../api/types';

export interface CatalogInfo extends CatalogModel {
  /** Service kind from the catalog, e.g. "chat" | "embedding". */
  kind: string;
}

export function indexCatalog(catalog: Catalog | null): Map<string, CatalogInfo> {
  const map = new Map<string, CatalogInfo>();
  if (!catalog) return map;
  for (const vendor of catalog.vendors) {
    for (const service of vendor.services) {
      for (const m of service.models) {
        if (!map.has(m.model)) map.set(m.model, { ...m, kind: service.kind });
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
