import type { CSSProperties } from 'react';
import { Link } from 'react-router-dom';
import { CheckCircle2, Plug, Plus, XCircle } from 'lucide-react';
import { api } from '../api/client';
import type { Provider } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { int, ms, percent } from '../lib/format';
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
              <Link to="/services/add">Add a provider</Link> to start routing models.
            </>
          }
        />
      ) : (
        <div className={styles.grid}>
          <Link to="/services/add" className={`card ${styles.addCard}`}>
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
  const { stats } = provider;
  const complete = provider.masked_key !== '' && provider.models.length > 0;

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
          <span className={`${styles.badge} ${stats.healthy ? styles.healthy : styles.unhealthy}`}>
            {stats.healthy ? <CheckCircle2 size={12} /> : <XCircle size={12} />}
            {stats.healthy ? 'Healthy' : 'Unhealthy'}
          </span>
        )}
      </div>

      <div className={styles.baseUrl}>{provider.base_url}</div>

      <div className={styles.cardMeta}>
        <span className="chip">{provider.adapter}</span>
        <span className="chip">
          {provider.models.length} {provider.models.length === 1 ? 'model' : 'models'}
        </span>
        {provider.vendor && <span className="chip">{provider.vendor}</span>}
      </div>

      <span className={styles.statsRow}>
        {stats.requests > 0
          ? `${int(stats.requests)} requests · ${percent(stats.error_rate)} errors · ${ms(stats.avg_latency_ms)}`
          : 'No traffic yet'}
      </span>
    </Link>
  );
}
