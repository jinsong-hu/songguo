import { useMemo, type CSSProperties } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { AlertTriangle, Check } from 'lucide-react';
import { api } from '../api/client';
import type { CatalogVendor, Provider, ProviderRouting } from '../api/types';
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
        ) : (
          <RoutingBadge routing={provider.routing} />
        )}
      </div>
      <ProviderFooter provider={provider} />
    </Link>
  );
}

/**
 * Live routing health. Only rendered when something is wrong — a healthy
 * provider shows nothing, so anything visible here is worth reading.
 */
function RoutingBadge({ routing }: { routing?: ProviderRouting }) {
  if (!routing) return null;

  // Dead outranks cooling: it does not lapse on a timer, so it is the one that
  // needs an operator rather than patience.
  if (routing.dead) {
    return (
      <span className={`${styles.badge} ${styles.dead}`} title={degradedTitle(routing, 'Not being retried')}>
        <AlertTriangle size={11} /> Dead
      </span>
    );
  }
  if (routing.cooling) {
    return (
      <span className={`${styles.badge} ${styles.cooling}`} title={degradedTitle(routing, 'Temporarily demoted')}>
        Cooling
      </span>
    );
  }
  return null;
}

function degradedTitle(routing: ProviderRouting, lead: string): string {
  if (!routing.degraded?.length) return lead;
  return `${lead}: ${routing.degraded.join(', ')}`;
}

/**
 * The two live numbers worth surfacing per provider: how many agent sessions
 * are pinned here (prompt-cache locality) and how the concurrency limit is
 * doing. Both are omitted when they have nothing to say.
 */
function ProviderFooter({ provider }: { provider: Provider }) {
  const { capacity, routing } = provider;
  const sessions = routing?.sessions ?? 0;
  const showCapacity = capacity.limit > 0 || capacity.in_flight > 0 || capacity.waiting > 0;
  if (!sessions && !showCapacity) return null;

  return (
    <div className={styles.cardFoot}>
      {showCapacity && (
        <span
          className={capacity.waiting > 0 ? styles.metricBusy : styles.metric}
          title={
            capacity.waiting > 0
              ? `${capacity.waiting} request(s) queued for a slot. Requests wait rather than moving to another provider, so the session keeps its prompt cache — but a queue that is always busy means the limit is below what the provider allows.`
              : 'In-flight requests against this provider’s concurrency limit.'
          }
        >
          {capacity.limit > 0
            ? `${capacity.in_flight}/${capacity.limit} in flight`
            : `${capacity.in_flight} in flight`}
          {capacity.waiting > 0 ? ` · ${capacity.waiting} queued` : ''}
        </span>
      )}
      {sessions > 0 && (
        <span className={styles.metric} title="Agent sessions pinned here, keeping their prompt cache warm.">
          {sessions} session{sessions === 1 ? '' : 's'}
        </span>
      )}
    </div>
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
