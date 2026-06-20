import { useMemo, type CSSProperties } from 'react';
import { Link } from 'react-router-dom';
import { Layers } from 'lucide-react';
import { api } from '../api/client';
import type { Service } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { ErrorBanner } from '../components/ErrorBanner';
import { Page } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useFetch } from '../lib/useFetch';
import { ModelIcon, modelMeta } from '../lib/modelBrand';
import { BUCKETS, modelKind, type Kind } from '../lib/serviceModality';
import styles from './Services.module.css';

export function ServicesPage() {
  const { data, error, initialLoading, refetch } = useFetch(() => api.services(), []);
  const { data: catalog } = useFetch(() => api.catalog(), []);

  // Each service's coarse kind, resolved once from the catalog.
  const kinds = useMemo(() => {
    const map = new Map<string, Kind>();
    for (const s of data ?? []) map.set(s.model, modelKind(s.model, catalog));
    return map;
  }, [data, catalog]);

  // Group services into the Hugging Face task buckets; drop empty buckets, then
  // collect anything left over into a trailing "Other".
  const groups = useMemo(() => {
    const services = data ?? [];
    const placed = new Set<string>();
    const out: Array<{ id: string; label: string; items: Service[] }> = [];
    for (const b of BUCKETS) {
      const items = services.filter((s) => b.kinds.includes(kinds.get(s.model) ?? 'chat'));
      if (items.length === 0) continue;
      items.forEach((s) => placed.add(s.model));
      out.push({ id: b.id, label: b.label, items });
    }
    const rest = services.filter((s) => !placed.has(s.model));
    if (rest.length > 0) {
      out.push({ id: 'other', label: 'Other', items: rest });
    }
    return out;
  }, [data, kinds]);

  return (
    <Page title="Services">
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
              <Link to="/providers">Add a provider</Link> to start routing models.
            </>
          }
        />
      ) : (
        <div className={styles.groups}>
          {groups.map((g) => (
            <section key={g.id} className={styles.group}>
              <div className={styles.groupHead}>
                <h2 className={styles.groupTitle}>{g.label}</h2>
              </div>
              <div className={styles.grid}>
                {g.items.map((s) => (
                  <ModelCard key={s.model} service={s} />
                ))}
              </div>
            </section>
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
