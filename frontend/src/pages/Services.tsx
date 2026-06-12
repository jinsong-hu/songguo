import type { CSSProperties } from 'react';
import { Link } from 'react-router-dom';
import { Layers, Plus } from 'lucide-react';
import { api } from '../api/client';
import type { Service } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { ModelIcon, modelMeta } from '../lib/modelBrand';
import styles from './Services.module.css';

export function ServicesPage() {
  const { data, error, initialLoading, refetch } = useFetch(() => api.services(), []);

  return (
    <Page
      title="Services"
      actions={
        <Link to="/services/add" className="btn btn-primary">
          <Plus size={15} /> Add service
        </Link>
      }
    >
      {error ? (
        <ErrorBanner message={error} onRetry={refetch} />
      ) : initialLoading ? (
        <div className={styles.grid}>
          <Skeleton height={110} />
          <Skeleton height={110} />
          <Skeleton height={110} />
          <Skeleton height={110} />
        </div>
      ) : !data || data.length === 0 ? (
        <EmptyState
          icon={Layers}
          title="No services yet"
          hint={
            <>
              <Link to="/services/add">Add a provider</Link> to start routing models.
            </>
          }
        />
      ) : (
        <div className={styles.grid}>
          {data.map((s) => (
            <ModelCard key={s.model} service={s} />
          ))}
        </div>
      )}
    </Page>
  );
}

function ModelCard({ service }: { service: Service }) {
  const meta = modelMeta(service.model);

  return (
    <Link
      to={`/services/${encodeURIComponent(service.model)}`}
      className={`card ${styles.modelCard}`}
      style={{ '--brand': meta.color } as CSSProperties}
    >
      <div className={styles.cardHead}>
        <span className={styles.iconTile}>
          <ModelIcon model={service.model} size={22} />
        </span>
        <span className={styles.cardName}>{meta.name}</span>
      </div>
      <div className={styles.cardTagline}>{meta.tagline}</div>
    </Link>
  );
}
