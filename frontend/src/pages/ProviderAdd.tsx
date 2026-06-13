import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { ArrowLeft, Check, Search, Wrench } from 'lucide-react';
import { api } from '../api/client';
import type { CatalogVendor } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { BrandIcon, providerBrand } from '../lib/modelBrand';
import { wireName, wireServesModels } from '../lib/wires';
import styles from './ProviderAdd.module.css';

// Distinct capability wires a vendor offers (chat/responses/embeddings/messages/
// tts/voice-clone), dropping companion wires like model listings.
function capabilityWires(vendor: CatalogVendor): string[] {
  const seen: string[] = [];
  for (const ep of vendor.endpoints) {
    if (wireServesModels(ep.wire) && !seen.includes(ep.wire)) seen.push(ep.wire);
  }
  return seen;
}

export function ProviderAddPage() {
  const catalog = useFetch(() => api.catalog(), []);
  // Used to mark vendors that already have a configured provider.
  const providers = useFetch(() => api.providers(), []);
  const [query, setQuery] = useState('');
  const navigate = useNavigate();

  const addedVendorIds = useMemo(() => {
    const set = new Set<string>();
    for (const p of providers.data ?? []) if (p.catalog_id) set.add(p.catalog_id);
    return set;
  }, [providers.data]);

  const vendors = useMemo<CatalogVendor[]>(() => {
    const q = query.trim().toLowerCase();
    return (catalog.data?.vendors ?? []).filter((v) => {
      if (!q) return true;
      if (v.name.toLowerCase().includes(q)) return true;
      if (v.endpoints.some((ep) => wireName(ep.wire).toLowerCase().includes(q))) return true;
      return Object.keys(v.models).some((m) => m.toLowerCase().includes(q));
    });
  }, [catalog.data, query]);

  return (
    <Page
      title="Add provider"
      actions={
        <Link to="/providers" className="btn">
          <ArrowLeft size={15} /> Back to providers
        </Link>
      }
    >
      {catalog.error ? (
        <ErrorBanner message={catalog.error} onRetry={catalog.refetch} />
      ) : catalog.initialLoading ? (
        <div className={styles.grid}>
          {Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className={`card ${styles.entry}`} style={{ padding: 16 }}>
              <Skeleton height={18} width={160} />
              <Skeleton height={13} width="70%" style={{ marginTop: 10 }} />
            </div>
          ))}
        </div>
      ) : (
        <>
          <div className={styles.searchBox}>
            <Search size={15} />
            <input
              className={styles.searchInput}
              value={query}
              placeholder="Search vendors, models, wires…"
              onChange={(e) => setQuery(e.target.value)}
            />
          </div>

          {vendors.length === 0 && query.trim() !== '' ? (
            <EmptyState icon={Search} title="No matches" hint="Try a different search." />
          ) : (
            <div className={styles.grid}>
              {vendors.map((vendor) => (
                <VendorTile
                  key={vendor.id}
                  vendor={vendor}
                  added={addedVendorIds.has(vendor.id)}
                  onOpen={() => navigate(`/providers/add/${encodeURIComponent(vendor.id)}`)}
                />
              ))}
              <button
                className={`card ${styles.entry} ${styles.custom}`}
                onClick={() => navigate('/providers/new')}
              >
                <div className={styles.customIcon}>
                  <Wrench size={18} />
                </div>
                <span className={styles.serviceName}>Custom provider</span>
                <span className={styles.note}>
                  Any OpenAI- or Anthropic-compatible endpoint: set the base URL, key, wires, and
                  per-model prices yourself.
                </span>
              </button>
            </div>
          )}
        </>
      )}
    </Page>
  );
}

interface VendorTileProps {
  vendor: CatalogVendor;
  added: boolean;
  onOpen: () => void;
}

function VendorTile({ vendor, added, onOpen }: VendorTileProps) {
  const caps = capabilityWires(vendor);
  return (
    <button className={`card ${styles.entry} ${styles.vendorTile}`} onClick={onOpen}>
      <div className={styles.entryHead}>
        <span className={styles.vendorTitle}>
          <BrandIcon
            brand={providerBrand(vendor.name, Object.keys(vendor.models))}
            label={vendor.name}
            size={20}
          />
          <span className={styles.serviceName}>{vendor.name}</span>
        </span>
        {added && (
          <span className={styles.addedChip}>
            <Check size={12} /> Added
          </span>
        )}
      </div>
      <div className={styles.tags}>
        {caps.map((w) => (
          <span key={w} className="chip">
            {wireName(w)}
          </span>
        ))}
      </div>
    </button>
  );
}
