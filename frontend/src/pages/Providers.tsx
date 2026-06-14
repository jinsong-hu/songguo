import { useMemo, type CSSProperties } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { Check } from 'lucide-react';
import { api } from '../api/client';
import type { CatalogVendor, Provider } from '../api/types';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { BrandIcon, providerBrand } from '../lib/modelBrand';
import styles from './Providers.module.css';

export function ProvidersPage() {
  const providers = useFetch(() => api.providers(), []);
  const catalog = useFetch(() => api.catalog(), []);
  const navigate = useNavigate();

  const error = providers.error || catalog.error;
  const initialLoading = providers.initialLoading || catalog.initialLoading;

  // Vendors that already back a configured provider, so we can mark them "Added".
  const addedVendorIds = useMemo(() => {
    const set = new Set<string>();
    for (const p of providers.data ?? []) if (p.catalog_id) set.add(p.catalog_id);
    return set;
  }, [providers.data]);

  const refetch = () => {
    providers.refetch();
    catalog.refetch();
  };

  const existing = providers.data ?? [];
  const vendors = catalog.data?.vendors ?? [];

  return (
    <Page title="Providers">
      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading ? (
        <div className={styles.grid}>
          <Skeleton height={70} />
          <Skeleton height={70} />
          <Skeleton height={70} />
          <Skeleton height={70} />
        </div>
      ) : (
        <>
          {existing.length > 0 && (
            <div className={styles.grid}>
              {existing.map((p) => (
                <ProviderCard key={p.id} provider={p} />
              ))}
            </div>
          )}

          <div className={styles.divider}>
            <span>Add a provider</span>
          </div>
          <div className={styles.grid}>
            {vendors.map((vendor) => (
              <VendorTile
                key={vendor.id}
                vendor={vendor}
                added={!vendor.custom && addedVendorIds.has(vendor.id)}
                onOpen={() => navigate(`/providers/add/${encodeURIComponent(vendor.id)}`)}
              />
            ))}
          </div>
        </>
      )}
    </Page>
  );
}

function ProviderCard({ provider }: { provider: Provider }) {
  const brand = providerBrand(
    provider.vendor,
    provider.models.map((m) => m.model),
  );
  const complete = provider.masked_key !== '' && provider.endpoints.length > 0;

  return (
    <Link
      to={`/providers/${provider.id}/edit`}
      className={`card ${styles.providerCard} ${provider.enabled ? '' : styles.disabled}`}
      style={{ '--brand': brand?.color ?? '#3f8f5b' } as CSSProperties}
    >
      <div className={styles.cardHead}>
        <span className={styles.iconTile}>
          <BrandIcon brand={brand} label={provider.name} size={22} />
        </span>
        <span className={styles.cardName}>{provider.name}</span>
        {!provider.enabled ? (
          <span className={`${styles.badge} ${styles.off}`}>Disabled</span>
        ) : !complete ? (
          <span className={`${styles.badge} ${styles.draft}`}>Draft</span>
        ) : null}
      </div>
    </Link>
  );
}

interface VendorTileProps {
  vendor: CatalogVendor;
  added: boolean;
  onOpen: () => void;
}

function VendorTile({ vendor, added, onOpen }: VendorTileProps) {
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
    </button>
  );
}
