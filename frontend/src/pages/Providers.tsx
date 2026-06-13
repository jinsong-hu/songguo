import type { CSSProperties } from 'react';
import { Link } from 'react-router-dom';
import { Plug, Plus } from 'lucide-react';
import { api } from '../api/client';
import type { Provider } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { BrandIcon, providerBrand } from '../lib/modelBrand';
import styles from './Providers.module.css';

export function ProvidersPage() {
  const { data, error, initialLoading, refetch } = useFetch(() => api.providers(), []);

  return (
    <Page title="Providers">
      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading ? (
        <div className={styles.grid}>
          <Skeleton height={130} />
          <Skeleton height={130} />
          <Skeleton height={130} />
          <Skeleton height={130} />
        </div>
      ) : !data || data.length === 0 ? (
        <EmptyState
          icon={Plug}
          title="No providers yet"
          hint={
            <>
              <Link to="/providers/add">Add a provider</Link> to start routing models.
            </>
          }
        />
      ) : (
        <div className={styles.grid}>
          <Link to="/providers/add" className={`card ${styles.addCard}`}>
            <Plus size={20} />
            <span>Add provider</span>
          </Link>
          {data.map((p) => (
            <ProviderCard key={p.id} provider={p} />
          ))}
        </div>
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
